package olm

import (
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/apis/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/informers/externalversions"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/annotator"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/install"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/queueinformer"
)

var ErrRequirementsNotMet = errors.New("requirements were not met")

const (
	FallbackWakeupInterval = 30 * time.Second
)

type Operator struct {
	*queueinformer.Operator
	csvQueue  workqueue.RateLimitingInterface
	client    versioned.Interface
	resolver  install.StrategyResolverInterface
	annotator *annotator.Annotator
}

func NewOperator(kubeconfig string, wakeupInterval time.Duration, annotations map[string]string, namespaces []string) (*Operator, error) {
	if wakeupInterval < 0 {
		wakeupInterval = FallbackWakeupInterval
	}
	if len(namespaces) < 1 {
		namespaces = []string{metav1.NamespaceAll}
	}

	// Create a new client for ALM types (CRs)
	crClient, err := client.NewClient(kubeconfig)
	if err != nil {
		return nil, err
	}

	queueOperator, err := queueinformer.NewOperator(kubeconfig)
	if err != nil {
		return nil, err
	}
	namespaceAnnotator := annotator.NewAnnotator(queueOperator.OpClient, annotations)

	op := &Operator{
		Operator:  queueOperator,
		client:    crClient,
		resolver:  &install.StrategyResolver{},
		annotator: namespaceAnnotator,
	}

	// if watching all namespaces, set up a watch to annotate new namespaces
	if len(namespaces) == 1 && namespaces[0] == metav1.NamespaceAll {
		log.Debug("watching all namespaces, setting up queue")
		namespaceInformer := informers.NewSharedInformerFactory(queueOperator.OpClient.KubernetesInterface(), wakeupInterval).Core().V1().Namespaces().Informer()
		queueInformer := queueinformer.NewInformer(
			workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "namespaces"),
			namespaceInformer,
			op.annotateNamespace,
			nil,
		)
		op.RegisterQueueInformer(queueInformer)
	}

	// annotate namespaces that ALM operator manages
	if err := namespaceAnnotator.AnnotateNamespaces(namespaces); err != nil {
		return nil, err
	}

	// set up watch on CSVs
	csvInformers := []cache.SharedIndexInformer{}
	for _, namespace := range namespaces {
		log.Debugf("watching for CSVs in namespace %s", namespace)
		sharedInformerFactory := externalversions.NewSharedInformerFactoryWithOptions(crClient, wakeupInterval, externalversions.WithNamespace(namespace))
		csvInformers = append(csvInformers, sharedInformerFactory.Operators().V1alpha1().ClusterServiceVersions().Informer())
	}

	// csvInformers for each namespace all use the same backing queue
	// queue keys are namespaced
	csvQueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "clusterserviceversions")
	queueInformers := queueinformer.New(
		csvQueue,
		csvInformers,
		op.syncClusterServiceVersion,
		nil,
	)
	for _, informer := range queueInformers {
		op.RegisterQueueInformer(informer)
	}
	op.csvQueue = csvQueue
	return op, nil
}

func (a *Operator) requeueCSV(csv *v1alpha1.ClusterServiceVersion) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(csv)
	if err != nil {
		log.Infof("creating key failed: %s", err)
		return
	}
	log.Infof("requeueing %s", csv.SelfLink)
	a.csvQueue.AddRateLimited(k)
	return
}

// syncClusterServiceVersion is the method that gets called when we see a CSV event in the cluster
func (a *Operator) syncClusterServiceVersion(obj interface{}) (syncError error) {
	clusterServiceVersion, ok := obj.(*v1alpha1.ClusterServiceVersion)
	if !ok {
		log.Debugf("wrong type: %#v", obj)
		return fmt.Errorf("casting ClusterServiceVersion failed")
	}
	logger := log.WithFields(log.Fields{
		"csv":       clusterServiceVersion.GetName(),
		"namespace": clusterServiceVersion.GetNamespace(),
	})
	logger.Info("syncing")

	outCSV, syncError := a.transitionCSVState(*clusterServiceVersion)

	// no changes in status, don't update
	if outCSV.Status.Phase == clusterServiceVersion.Status.Phase && outCSV.Status.Reason == clusterServiceVersion.Status.Reason && outCSV.Status.Message == clusterServiceVersion.Status.Message {
		return
	}

	// Update CSV with status of transition. Log errors if we can't write them to the status.
	_, err := a.client.OperatorsV1alpha1().ClusterServiceVersions(clusterServiceVersion.GetNamespace()).UpdateStatus(outCSV)
	if err != nil {
		updateErr := errors.New("error updating ClusterServiceVersion status: " + err.Error())
		if syncError == nil {
			logger.Info(updateErr)
			return updateErr
		}
		syncError = fmt.Errorf("error transitioning ClusterServiceVersion: %s and error updating CSV status: %s", syncError, updateErr)
	}
	return
}

// transitionCSVState moves the CSV status state machine along based on the current value and the current cluster
// state.
func (a *Operator) transitionCSVState(in v1alpha1.ClusterServiceVersion) (out *v1alpha1.ClusterServiceVersion, syncError error) {
	logger := log.WithFields(log.Fields{
		"csv":       in.GetName(),
		"namespace": in.GetNamespace(),
		"phase":     in.Status.Phase,
	})

	out = in.DeepCopy()

	// check if the current CSV is being replaced, return with replacing status if so
	if err := a.checkReplacementsAndUpdateStatus(out); err != nil {
		return
	}

	switch out.Status.Phase {
	case v1alpha1.CSVPhaseNone:
		logger.Infof("scheduling ClusterServiceVersion for requirement verification")
		out.SetPhase(v1alpha1.CSVPhasePending, v1alpha1.CSVReasonRequirementsUnknown, "requirements not yet checked")
	case v1alpha1.CSVPhasePending:
		met, statuses := a.requirementStatus(out)
		out.SetRequirementStatus(statuses)

		if !met {
			logger.Info("requirements were not met")
			out.SetPhase(v1alpha1.CSVPhasePending, v1alpha1.CSVReasonRequirementsNotMet, "one or more requirements couldn't be found")
			syncError = ErrRequirementsNotMet
			return
		}

		// check for CRD ownership conflicts
		if syncError = a.crdOwnerConflicts(out, a.csvsInNamespace(out.GetNamespace())); syncError != nil {
			out.SetPhase(v1alpha1.CSVPhaseFailed, v1alpha1.CSVReasonOwnerConflict, fmt.Sprintf("owner conflict: %s", syncError))
			return
		}

		logger.Info("scheduling ClusterServiceVersion for install")
		out.SetPhase(v1alpha1.CSVPhaseInstallReady, v1alpha1.CSVReasonRequirementsMet, "all requirements found, attempting install")
	case v1alpha1.CSVPhaseInstallReady:
		installer, strategy, _ := a.parseStrategiesAndUpdateStatus(out)
		if strategy == nil {
			// parseStrategiesAndUpdateStatus sets CSV status
			return
		}

		if syncError = installer.Install(strategy); syncError != nil {
			out.SetPhase(v1alpha1.CSVPhaseFailed, v1alpha1.CSVReasonComponentFailed, fmt.Sprintf("install strategy failed: %s", syncError))
			return
		}

		out.SetPhase(v1alpha1.CSVPhaseInstalling, v1alpha1.CSVReasonInstallSuccessful, "waiting for install components to report healthy")
		a.requeueCSV(out)
		return
	case v1alpha1.CSVPhaseInstalling:
		installer, strategy, _ := a.parseStrategiesAndUpdateStatus(out)
		if strategy == nil {
			// parseStrategiesAndUpdateStatus sets CSV status
			return
		}

		if installErr := a.updateInstallStatus(out, installer, strategy, v1alpha1.CSVReasonWaiting); installErr == nil {
			logger.WithField("strategy", out.Spec.InstallStrategy.StrategyName).Infof("install strategy successful")
		}

	case v1alpha1.CSVPhaseSucceeded:
		installer, strategy, _ := a.parseStrategiesAndUpdateStatus(out)
		if strategy == nil {
			// parseStrategiesAndUpdateStatus sets CSV status
			return
		}
		if installErr := a.updateInstallStatus(out, installer, strategy, v1alpha1.CSVReasonComponentUnhealthy); installErr != nil {
			logger.WithField("strategy", out.Spec.InstallStrategy.StrategyName).Infof("unhealthy component: %s", installErr)
		}
	case v1alpha1.CSVPhaseReplacing:
		// determine CSVs that are safe to delete by finding a replacement chain to a CSV that's running
		// since we don't know what order we'll process replacements, we have to guard against breaking that chain

		// if this isn't the earliest csv in a replacement chain, skip gc.
		// marking an intermediate for deletion will break the replacement chain
		if prev := a.isReplacing(out); prev != nil {
			logger.Debugf("being replaced, but is not a leaf. skipping gc")
			return
		}

		// if we can find a newer version that's successfully installed, we're safe to mark all intermediates
		for _, csv := range a.findIntermediatesForDeletion(out) {
			// TODO fix this
			// we only mark them in this step, in case some get deleted but others fail and break the replacement chain
			csv.SetPhase(v1alpha1.CSVPhaseDeleting, v1alpha1.CSVReasonReplaced, "has been replaced by a newer ClusterServiceVersion that has successfully installed.")
		}

		// if there's no newer version, requeue for processing (likely will be GCable before resync)
		a.requeueCSV(out)
	case v1alpha1.CSVPhaseDeleting:
		syncError := a.OpClient.DeleteCustomResource(v1alpha1.GroupName, v1alpha1.GroupVersion, out.GetNamespace(), v1alpha1.ClusterServiceVersionKind, out.GetName())
		if syncError != nil {
			logger.Debugf("unable to get delete csv marked for deletion: %s", syncError.Error())
		}
	}

	return
}

// findIntermediatesForDeletion starts at csv and follows the replacement chain until one is running and active
func (a *Operator) findIntermediatesForDeletion(csv *v1alpha1.ClusterServiceVersion) (csvs []*v1alpha1.ClusterServiceVersion) {
	csvsInNamespace := a.csvsInNamespace(csv.GetNamespace())
	current := csv
	next := a.isBeingReplaced(current, csvsInNamespace)
	for next != nil {
		csvs = append(csvs, current)
		log.Debugf("checking to see if %s is running so we can delete %s", next.GetName(), csv.GetName())
		installer, nextStrategy, currentStrategy := a.parseStrategiesAndUpdateStatus(next)
		if nextStrategy == nil {
			log.Debugf("couldn't get strategy for %s", next.GetName())
			continue
		}
		if currentStrategy == nil {
			log.Debugf("couldn't get strategy for %s", next.GetName())
			continue
		}
		installed, _ := installer.CheckInstalled(nextStrategy)
		if installed && !next.IsObsolete() {
			return csvs
		}
		current = next
		next = a.isBeingReplaced(current, csvsInNamespace)
	}
	return nil
}

// csvsInNamespace finds all CSVs in a namespace
func (a *Operator) csvsInNamespace(namespace string) (csvs []*v1alpha1.ClusterServiceVersion) {
	csvsInNamespace, err := a.OpClient.ListCustomResource(v1alpha1.GroupName, v1alpha1.GroupVersion, namespace, v1alpha1.ClusterServiceVersionKind)
	if err != nil {
		return nil
	}
	for _, csvUnst := range csvsInNamespace.Items {
		csv := v1alpha1.ClusterServiceVersion{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(csvUnst.UnstructuredContent(), &csv); err != nil {
			continue
		}
		csvs = append(csvs, &csv)
	}
	return
}

// checkReplacementsAndUpdateStatus returns an error if we can find a newer CSV and sets the status if so
func (a *Operator) checkReplacementsAndUpdateStatus(csv *v1alpha1.ClusterServiceVersion) error {
	if csv.Status.Phase == v1alpha1.CSVPhaseReplacing || csv.Status.Phase == v1alpha1.CSVPhaseDeleting {
		return nil
	}

	if replacement := a.isBeingReplaced(csv, a.csvsInNamespace(csv.GetNamespace())); replacement != nil {
		log.Infof("newer ClusterServiceVersion replacing %s, no-op", csv.SelfLink)
		msg := fmt.Sprintf("being replaced by csv: %s", replacement.SelfLink)
		csv.SetPhase(v1alpha1.CSVPhaseReplacing, v1alpha1.CSVReasonBeingReplaced, msg)

		// requeue so that we quickly pick up on replacement status changes
		a.requeueCSV(csv)

		return fmt.Errorf("replacing")
	}
	return nil
}

func (a *Operator) updateInstallStatus(csv *v1alpha1.ClusterServiceVersion, installer install.StrategyInstaller, strategy install.Strategy, requeueConditionReason v1alpha1.ConditionReason) error {
	installed, strategyErr := installer.CheckInstalled(strategy)
	if installed {
		// if there's no error, we're successfully running
		if csv.Status.Phase != v1alpha1.CSVPhaseSucceeded {
			csv.SetPhase(v1alpha1.CSVPhaseSucceeded, v1alpha1.CSVReasonInstallSuccessful, "install strategy completed with no errors")
		}
		return nil
	}

	// installcheck determined we can't progress (e.g. deployment failed to come up in time)
	if install.IsErrorUnrecoverable(strategyErr) {
		csv.SetPhase(v1alpha1.CSVPhaseFailed, v1alpha1.CSVReasonInstallCheckFailed, fmt.Sprintf("install failed: %s", strategyErr))
		return strategyErr
	}

	// if there's an error checking install that shouldn't fail the strategy, requeue with message
	if strategyErr != nil {
		csv.SetPhase(v1alpha1.CSVPhaseInstalling, requeueConditionReason, fmt.Sprintf("installing: %s", strategyErr))
		a.requeueCSV(csv)
		return strategyErr
	}

	return nil
}

// parseStrategiesAndUpdateStatus returns a StrategyInstaller and a Strategy for a CSV if it can, else it sets a status on the CSV and returns
func (a *Operator) parseStrategiesAndUpdateStatus(csv *v1alpha1.ClusterServiceVersion) (install.StrategyInstaller, install.Strategy, install.Strategy) {
	strategy, err := a.resolver.UnmarshalStrategy(csv.Spec.InstallStrategy)
	if err != nil {
		csv.SetPhase(v1alpha1.CSVPhaseFailed, v1alpha1.CSVReasonInvalidStrategy, fmt.Sprintf("install strategy invalid: %s", err))
		return nil, nil, nil
	}

	previousCSV := a.isReplacing(csv)
	var previousStrategy install.Strategy
	if previousCSV != nil {
		previousStrategy, err = a.resolver.UnmarshalStrategy(previousCSV.Spec.InstallStrategy)
		if err != nil {
			previousStrategy = nil
		}
	}
	if previousStrategy != nil {
		// check for status changes if we know we're replacing a CSV
		a.requeueCSV(previousCSV)
	}

	strName := strategy.GetStrategyName()
	installer := a.resolver.InstallerForStrategy(strName, a.OpClient, csv, previousStrategy)
	return installer, strategy, previousStrategy
}

func (a *Operator) requirementStatus(csv *v1alpha1.ClusterServiceVersion) (met bool, statuses []v1alpha1.RequirementStatus) {
	met = true
	for _, r := range csv.GetAllCRDDescriptions() {
		status := v1alpha1.RequirementStatus{
			Group:   "apiextensions.k8s.io",
			Version: "v1beta1",
			Kind:    "CustomResourceDefinition",
			Name:    r.Name,
		}
		crd, err := a.OpClient.ApiextensionsV1beta1Interface().ApiextensionsV1beta1().CustomResourceDefinitions().Get(r.Name, metav1.GetOptions{})
		if err != nil {
			status.Status = "NotPresent"
			met = false
		} else {
			status.Status = "Present"
			status.UUID = string(crd.GetUID())
		}
		statuses = append(statuses, status)
	}
	return
}

func (a *Operator) crdOwnerConflicts(in *v1alpha1.ClusterServiceVersion, csvsInNamespace []*v1alpha1.ClusterServiceVersion) error {
	for _, crd := range in.Spec.CustomResourceDefinitions.Owned {
		for _, csv := range csvsInNamespace {
			if csv.OwnsCRD(crd.Name) {
				// two csvs own the same CRD, only valid if there's a replacing chain between them
				// TODO: this and the other replacement checking should just load the replacement chain DAG into memory
				current := csv
				for {
					if in.Spec.Replaces == current.GetName() {
						return nil
					}
					next := a.isBeingReplaced(current, csvsInNamespace)
					if next != nil {
						current = next
						continue
					}
					if in.Name == csv.Name {
						return nil
					}
					// couldn't find a chain between the two csvs
					return fmt.Errorf("%s and %s both own %s, but there is no replacement chain linking them", in.Name, csv.Name, crd.Name)
				}
			}
		}
	}
	return nil
}

// annotateNamespace is the method that gets called when we see a namespace event in the cluster
func (a *Operator) annotateNamespace(obj interface{}) (syncError error) {
	namespace, ok := obj.(*corev1.Namespace)
	if !ok {
		log.Debugf("wrong type: %#v", obj)
		return fmt.Errorf("casting Namespace failed")
	}

	log.Infof("syncing Namespace: %s", namespace.GetName())
	if err := a.annotator.AnnotateNamespace(namespace); err != nil {
		log.Infof("error annotating namespace '%s'", namespace.GetName())
		return err
	}
	return nil
}

func (a *Operator) isBeingReplaced(in *v1alpha1.ClusterServiceVersion, csvsInNamespace []*v1alpha1.ClusterServiceVersion) (replacedBy *v1alpha1.ClusterServiceVersion) {
	for _, csv := range csvsInNamespace {
		if csv.Spec.Replaces == in.GetName() {
			replacedBy = csv
			return
		}
	}
	return
}

func (a *Operator) isReplacing(in *v1alpha1.ClusterServiceVersion) (previous *v1alpha1.ClusterServiceVersion) {
	log.Debugf("checking if csv is replacing an older version")
	if in.Spec.Replaces == "" {
		return nil
	}
	oldCSVUnst, err := a.OpClient.GetCustomResource(v1alpha1.GroupName, v1alpha1.GroupVersion, in.GetNamespace(), v1alpha1.ClusterServiceVersionKind, in.Spec.Replaces)
	if err != nil {
		log.Debugf("unable to get previous csv: %s", err.Error())
		return nil
	}
	p := v1alpha1.ClusterServiceVersion{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(oldCSVUnst.UnstructuredContent(), &p); err != nil {
		log.Debugf("unable to parse previous csv: %s", err.Error())
		return nil
	}
	return &p
}
