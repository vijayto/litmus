/*
Copyright 2019 LitmusChaos Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package chaosengine

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/litmuschaos/elves/kubernetes/container"
	"github.com/litmuschaos/elves/kubernetes/pod"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/litmuschaos/chaos-operator/pkg/analytics"
	litmuschaosv1alpha1 "github.com/litmuschaos/chaos-operator/pkg/apis/litmuschaos/v1alpha1"
	"github.com/litmuschaos/chaos-operator/pkg/controller/resource"
	chaosTypes "github.com/litmuschaos/chaos-operator/pkg/controller/types"
	"github.com/litmuschaos/chaos-operator/pkg/controller/utils"
	"github.com/litmuschaos/chaos-operator/pkg/controller/watcher"
)

const finalizer = "chaosengine.litmuschaos.io/finalizer"

var _ reconcile.Reconciler = &ReconcileChaosEngine{}

// ReconcileChaosEngine reconciles a ChaosEngine object
type ReconcileChaosEngine struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// reconcileEngine contains details of reconcileEngine
type reconcileEngine struct {
	r         *ReconcileChaosEngine
	reqLogger logr.Logger
}

//podEngineRunner contains the information of pod
type podEngineRunner struct {
	pod, engineRunner *corev1.Pod
	*reconcileEngine
}

// Add creates a new ChaosEngine Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileChaosEngine{client: mgr.GetClient(), scheme: mgr.GetScheme(), recorder: mgr.GetEventRecorderFor("chaos-operator")}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	c, err := controller.New("chaosengine-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	err = watchChaosResources(mgr.GetClient(), c)
	if err != nil {
		return err
	}
	return nil
}

var engine *chaosTypes.EngineInfo

// watchSecondaryResources watch's for changes in chaos resources
func watchChaosResources(clientSet client.Client, c controller.Controller) error {
	// Watch for Primary Chaos Resource
	err := c.Watch(&source.Kind{Type: &litmuschaosv1alpha1.ChaosEngine{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	//Watch for Secondary Chaos Resources
	err = watcher.WatchForRunnerPod(clientSet, c)
	if err != nil {
		return err
	}
	return nil
}

// Reconcile reads that state of the cluster for a ChaosEngine object and makes changes based on the state read
// and what is in the ChaosEngine.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileChaosEngine) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := startReqLogger(request)

	err := r.getChaosEngineInstance(request)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	//Handle deletion of Chaos Engine
	if engine.Instance.ObjectMeta.GetDeletionTimestamp() != nil {
		return r.reconcileForDelete(request)
	}

	// Start the reconcile by setting default values into Chaos Engine
	if err := r.initEngine(engine); err != nil {
		return reconcile.Result{}, err
	}

	// Handling of normal execution of Chaos Engine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusInitialized {
		return r.reconcileForCreationAndRunning(engine, reqLogger)
	}

	// Handling Graceful completion of Chaos Engine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateStop && engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusCompleted {
		return r.reconcileForComplete(request)
	}

	// Handling forceful Abort of Chaos Engine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateStop && engine.Instance.Status.EngineStatus != litmuschaosv1alpha1.EngineStatusCompleted {
		return r.reconcileForDelete(request)
	}

	// Handling restarting of Chaos Engine
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && (engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusCompleted || engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusStopped) {
		return r.reconcileForRestart(request)
	}

	return reconcile.Result{}, nil
}

// getChaosRunnerENV return the env required for chaos-runner
func getChaosRunnerENV(cr *litmuschaosv1alpha1.ChaosEngine, aExList []string, ClientUUID string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  "CHAOSENGINE",
			Value: cr.Name,
		},
		{
			Name:  "APP_LABEL",
			Value: cr.Spec.Appinfo.Applabel,
		},
		{
			Name:  "APP_NAMESPACE",
			Value: cr.Spec.Appinfo.Appns,
		},
		{
			Name:  "EXPERIMENT_LIST",
			Value: fmt.Sprint(strings.Join(aExList, ",")),
		},
		{
			Name:  "CHAOS_SVC_ACC",
			Value: cr.Spec.ChaosServiceAccount,
		},
		{
			Name:  "AUXILIARY_APPINFO",
			Value: cr.Spec.AuxiliaryAppInfo,
		},
		{
			Name:  "CLIENT_UUID",
			Value: ClientUUID,
		},
		{
			Name:  "CHAOS_NAMESPACE",
			Value: cr.Namespace,
		},
	}
}

// newRunnerPodForCR defines secondary resource #1 in same namespace as CR
func newRunnerPodForCR(engine *chaosTypes.EngineInfo) (*corev1.Pod, error) {
	if (len(engine.AppExperiments) == 0 || engine.AppUUID == "") && engine.Instance.Spec.AnnotationCheck == "true" {
		return nil, errors.New("application experiment list or UUID is empty")
	}
	//Initiate the Engine Info, with the type of runner to be used
	if engine.Instance.Spec.Components.Runner.Type == "ansible" {
		return newAnsibleRunnerPodForCR(engine)
	}
	return newGoRunnerPodForCR(engine)
}

// newGoRunnerPodForCR defines a new go-based Runner Pod
func newGoRunnerPodForCR(engine *chaosTypes.EngineInfo) (*corev1.Pod, error) {
	containerForRunner := container.NewBuilder().
		WithEnvsNew(getChaosRunnerENV(engine.Instance, engine.AppExperiments, analytics.ClientUUID)).
		WithName("chaos-runner").
		WithImage(engine.Instance.Spec.Components.Runner.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent)

	if engine.Instance.Spec.Components.Runner.ImagePullPolicy != "" {
		containerForRunner.WithImagePullPolicy(engine.Instance.Spec.Components.Runner.ImagePullPolicy)
	}

	if engine.Instance.Spec.Components.Runner.Args != nil {
		containerForRunner.WithArgumentsNew(engine.Instance.Spec.Components.Runner.Args)
	}

	if engine.Instance.Spec.Components.Runner.Command != nil {
		containerForRunner.WithCommandNew(engine.Instance.Spec.Components.Runner.Command)
	}

	// Proposed changes: Vijay Thomas - Intuit
	return pod.NewBuilder().
		WithName(engine.Instance.Name + "-runner").
		WithAnnotations(map[string]string{engine.Instance.Spec.AppAnnotations.AppAnnotationName:
			engine.Instance.Spec.AppAnnotations.AppAnnotationValue}).
		WithNamespace(engine.Instance.Namespace).
		WithLabels(map[string]string{"app": engine.Instance.Name, "chaosUID": string(engine.Instance.UID)}).
		WithServiceAccountName(engine.Instance.Spec.ChaosServiceAccount).
		WithRestartPolicy("OnFailure").
		WithContainerBuilder(containerForRunner).Build()
}

// newAnsibleRunnerPodForCR defines a new ansible-based Runner Pod
func newAnsibleRunnerPodForCR(engine *chaosTypes.EngineInfo) (*corev1.Pod, error) {
	containerForRunner := container.NewBuilder().
		WithName("chaos-runner").
		WithImage(engine.Instance.Spec.Components.Runner.Image).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithCommandNew([]string{"/bin/bash"}).
		WithArgumentsNew([]string{"-c", "ansible-playbook ./executor/test.yml -i /etc/ansible/hosts; exit 0"}).
		WithEnvsNew(getChaosRunnerENV(engine.Instance, engine.AppExperiments, analytics.ClientUUID))

	if engine.Instance.Spec.Components.Runner.ImagePullPolicy != "" {
		containerForRunner.WithImagePullPolicy(engine.Instance.Spec.Components.Runner.ImagePullPolicy)
	}

	if engine.Instance.Spec.Components.Runner.Args != nil {
		containerForRunner.WithArgumentsNew(engine.Instance.Spec.Components.Runner.Args)
	}

	if engine.Instance.Spec.Components.Runner.Command != nil {
		containerForRunner.WithCommandNew(engine.Instance.Spec.Components.Runner.Command)
	}

	return pod.NewBuilder().
		WithName(engine.Instance.Name + "-runner").
		WithLabels(map[string]string{"app": engine.Instance.Name, "chaosUID": string(engine.Instance.UID)}).
		WithNamespace(engine.Instance.Namespace).
		WithRestartPolicy("OnFailure").
		WithServiceAccountName(engine.Instance.Spec.ChaosServiceAccount).
		WithContainerBuilder(containerForRunner).
		Build()
}

// initializeApplicationInfo to initialize application info
func initializeApplicationInfo(instance *litmuschaosv1alpha1.ChaosEngine, appInfo *chaosTypes.ApplicationInfo) (*chaosTypes.ApplicationInfo, error) {
	if instance == nil {
		return nil, errors.New("empty chaosengine")
	}
	appLabel := strings.Split(instance.Spec.Appinfo.Applabel, "=")
	chaosTypes.AppLabelKey = appLabel[0]
	chaosTypes.AppLabelValue = appLabel[1]
	appInfo.Label = make(map[string]string)
	appInfo.Label[chaosTypes.AppLabelKey] = chaosTypes.AppLabelValue
	appInfo.Namespace = instance.Spec.Appinfo.Appns
	appInfo.ExperimentList = instance.Spec.Experiments
	appInfo.ServiceAccountName = instance.Spec.ChaosServiceAccount
	appInfo.Kind = instance.Spec.Appinfo.AppKind
	return appInfo, nil
}

// engineRunnerPod to Check if the engineRunner pod already exists, else create
func engineRunnerPod(runnerPod *podEngineRunner) error {
	err := runnerPod.r.client.Get(context.TODO(), types.NamespacedName{Name: runnerPod.engineRunner.Name, Namespace: runnerPod.engineRunner.Namespace}, runnerPod.pod)
	if err != nil && k8serrors.IsNotFound(err) {
		runnerPod.reqLogger.Info("Creating a new engineRunner Pod", "Pod.Namespace", runnerPod.engineRunner.Namespace, "Pod.Name", runnerPod.engineRunner.Name)
		err = runnerPod.r.client.Create(context.TODO(), runnerPod.engineRunner)
		if err != nil {
			return err
		}

		// Pod created successfully - don't requeue
		runnerPod.reqLogger.Info("engineRunner Pod created successfully")
	} else if err != nil {
		return err
	}
	runnerPod.reqLogger.Info("Skip reconcile: engineRunner Pod already exists", "Pod.Namespace", runnerPod.pod.Namespace, "Pod.Name", runnerPod.pod.Name)
	return nil
}

// Fetch the ChaosEngine instance
func (r *ReconcileChaosEngine) getChaosEngineInstance(request reconcile.Request) error {
	instance := &litmuschaosv1alpha1.ChaosEngine{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		// Error reading the object - requeue the request.
		return err
	}
	engine = &chaosTypes.EngineInfo{
		Instance: instance,
	}
	return nil
}

// Get application details
func getApplicationDetail(engine *chaosTypes.EngineInfo) error {
	applicationInfo := &chaosTypes.ApplicationInfo{}
	appInfo, err := initializeApplicationInfo(engine.Instance, applicationInfo)
	if err != nil {
		return err
	}
	engine.AppInfo = appInfo

	var appExperiments []string
	for _, exp := range appInfo.ExperimentList {
		appExperiments = append(appExperiments, exp.Name)
	}
	engine.AppExperiments = appExperiments

	chaosTypes.Log.Info("App key derived from chaosengine is ", "appLabelKey", chaosTypes.AppLabelKey)
	chaosTypes.Log.Info("App Label derived from Chaosengine is ", "appLabelValue", chaosTypes.AppLabelValue)
	chaosTypes.Log.Info("App NS derived from Chaosengine is ", "appNamespace", appInfo.Namespace)
	chaosTypes.Log.Info("Exp list derived from chaosengine is ", "appExpirements", appExperiments)
	chaosTypes.Log.Info("Monitoring Status derived from chaosengine is", "monitoringStatus", engine.Instance.Spec.Monitoring)
	chaosTypes.Log.Info("Runner image derived from chaosengine is", "runnerImage", engine.Instance.Spec.Components.Runner.Image)
	chaosTypes.Log.Info("Annotation check is ", "annotationCheck", engine.Instance.Spec.AnnotationCheck)
	return nil
}

// Check if the engineRunner pod already exists, else create
func (r *ReconcileChaosEngine) checkEngineRunnerPod(engine *chaosTypes.EngineInfo, reqLogger logr.Logger) error {
	if (len(engine.AppExperiments) == 0 || engine.AppUUID == "") && engine.Instance.Spec.AnnotationCheck == "true" {
		return errors.New("application experiment list or UUID is empty")
	}
	var engineRunner *corev1.Pod
	engineRunner, err := newGoRunnerPodForCR(engine)
	if err != nil {
		return err
	}
	//Initiate the Engine Info, with the type of runner to be used
	if engine.Instance.Spec.Components.Runner.Type == "ansible" {
		engineRunner, err = newAnsibleRunnerPodForCR(engine)
		if err != nil {
			return err
		}
	}

	// Create an object of engine reconcile.
	engineReconcile := &reconcileEngine{
		r:         r,
		reqLogger: reqLogger,
	}
	// Creates an object of engineRunner Pod
	runnerPod := &podEngineRunner{
		pod:             &corev1.Pod{},
		engineRunner:    engineRunner,
		reconcileEngine: engineReconcile,
	}

	if err := engineRunnerPod(runnerPod); err != nil {
		return err
	}
	return nil
}

//setChaosResourceImage take the runner image from engine spec
//if it is not there then it will take from chaos-operator env
//at last if it is not able to find image in engine spec and operator env then it will take default images
func setChaosResourceImage() {

	ChaosRunnerImage := os.Getenv("CHAOS_RUNNER_IMAGE")

	if engine.Instance.Spec.Components.Runner.Image == "" && ChaosRunnerImage == "" {
		engine.Instance.Spec.Components.Runner.Image = chaosTypes.DefaultChaosRunnerImage
	} else if engine.Instance.Spec.Components.Runner.Image == "" {
		engine.Instance.Spec.Components.Runner.Image = ChaosRunnerImage
	}
}

// getAnnotationCheck() checks for annotation on the application
func getAnnotationCheck() error {

	if engine.Instance.Spec.AnnotationCheck == "" {
		engine.Instance.Spec.AnnotationCheck = chaosTypes.DefaultAnnotationCheck

	}
	if engine.Instance.Spec.AnnotationCheck != "true" && engine.Instance.Spec.AnnotationCheck != "false" {
		return fmt.Errorf("annotationCheck '%s', is not supported it should be true or false", engine.Instance.Spec.AnnotationCheck)
	}
	return nil
}

// reconcileForDelete reconciles for deletion/force deletion of Chaos Engine
func (r *ReconcileChaosEngine) reconcileForDelete(request reconcile.Request) (reconcile.Result, error) {
	err := r.forceRemoveChaosResources(engine, request)
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to delete chaos resources")
		return reconcile.Result{}, err
	}
	opts := client.UpdateOptions{}

	if engine.Instance.ObjectMeta.Finalizers != nil {
		engine.Instance.ObjectMeta.Finalizers = utils.RemoveString(engine.Instance.ObjectMeta.Finalizers, "chaosengine.litmuschaos.io/finalizer")
		r.recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineStopped", " Chaos resources deleted successfully")
	}
	// Update ChaosEngine ExperimentStatuses, with aborted Status.
	updateExperimentStatusesForStop(engine)
	engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusStopped

	if err := r.client.Update(context.TODO(), engine.Instance, &opts); err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to update chaosengine")
		return reconcile.Result{}, fmt.Errorf("Unable to remove Finalizer from chaosEngine Resource, due to error: %v", err)
	}
	return reconcile.Result{}, nil

}

// forceRemoveAllChaosPods force removes all chaos-related pods
func (r *ReconcileChaosEngine) forceRemoveAllChaosPods(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	optsDelete := []client.DeleteAllOfOption{client.InNamespace(request.NamespacedName.Namespace), client.MatchingLabels{"chaosUID": string(engine.Instance.UID)}, client.PropagationPolicy(metav1.DeletePropagationBackground), client.GracePeriodSeconds(int64(0))}
	var deleteEvent []string
	var err []error

	if errDeployment := r.client.DeleteAllOf(context.TODO(), &appsv1.Deployment{}, optsDelete...); errDeployment != nil {
		err = append(err, errDeployment)
		deleteEvent = append(deleteEvent, "Deployments, ")
	}

	if errDaemonSet := r.client.DeleteAllOf(context.TODO(), &appsv1.DaemonSet{}, optsDelete...); errDaemonSet != nil {
		err = append(err, errDaemonSet)
		deleteEvent = append(deleteEvent, "DaemonSets, ")
	}

	if errJob := r.client.DeleteAllOf(context.TODO(), &batchv1.Job{}, optsDelete...); errJob != nil {
		err = append(err, errJob)
		deleteEvent = append(deleteEvent, "Jobs, ")
	}

	if errPod := r.client.DeleteAllOf(context.TODO(), &corev1.Pod{}, optsDelete...); errPod != nil {
		err = append(err, errPod)
		deleteEvent = append(deleteEvent, "Pods, ")
	}
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to delete chaos resources: %v allocated to chaosengine", strings.Join(deleteEvent, ""))
		return fmt.Errorf("Unable to delete ChaosResources due to %v", err)
	}
	return nil
}

// updateEngineState updates Chaos Engine Status with given State
func (r *ReconcileChaosEngine) updateEngineState(engine *chaosTypes.EngineInfo, state litmuschaosv1alpha1.EngineState) error {
	opts := client.UpdateOptions{}
	engine.Instance.Spec.EngineState = state
	return r.client.Update(context.TODO(), engine.Instance, &opts)
}

// checkRunnerPodCompletedStatus checks runner-pod status for Completed
func (r *ReconcileChaosEngine) checkRunnerPodCompletedStatus(engine *chaosTypes.EngineInfo) bool {
	runnerPod := corev1.Pod{}
	r.client.Get(context.TODO(), types.NamespacedName{Name: engine.Instance.Name + "-runner", Namespace: engine.Instance.Namespace}, &runnerPod)
	return runnerPod.Status.Phase == corev1.PodSucceeded
}

// gracefullyRemoveDefaultChaosResources removes all chaos-resources gracefully
func (r *ReconcileChaosEngine) gracefullyRemoveDefaultChaosResources(request reconcile.Request) (reconcile.Result, error) {

	if engine.Instance.Spec.JobCleanUpPolicy == litmuschaosv1alpha1.CleanUpPolicyDelete {
		if err := r.gracefullyRemoveChaosPods(engine, request); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// gracefullyRemoveChaosPods removes chaos default resources gracefully
func (r *ReconcileChaosEngine) gracefullyRemoveChaosPods(engine *chaosTypes.EngineInfo, request reconcile.Request) error {

	optsList := []client.ListOption{
		client.InNamespace(request.NamespacedName.Namespace), client.MatchingLabels{"app": engine.Instance.Name, "chaosUID": string(engine.Instance.UID)},
	}
	var podList corev1.PodList
	if errList := r.client.List(context.TODO(), &podList, optsList...); errList != nil {
		return errList
	}
	for _, v := range podList.Items {
		if errDel := r.client.Delete(context.TODO(), &v, []client.DeleteOption{}...); errDel != nil {
			return errDel
		}
	}
	return nil
}

// reconcileForComplete reconciles for graceful completion of Chaos Engine
func (r *ReconcileChaosEngine) reconcileForComplete(request reconcile.Request) (reconcile.Result, error) {

	_, err := r.gracefullyRemoveDefaultChaosResources(request)
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to delete chaos resources")
		return reconcile.Result{}, err
	}
	err = r.updateEngineState(engine, litmuschaosv1alpha1.EngineStateStop)
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to update chaosengine")
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// reconcileForRestart reconciles for restart of Chaos Engine
func (r *ReconcileChaosEngine) reconcileForRestart(request reconcile.Request) (reconcile.Result, error) {
	err := r.forceRemoveChaosResources(engine, request)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err = r.updateEngineForRestart(engine); err != nil {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, nil

}

// initEngine initilizes Chaos Engine, and add a finalizer to it.
func (r *ReconcileChaosEngine) initEngine(engine *chaosTypes.EngineInfo) error {
	if engine.Instance.Spec.EngineState == "" {
		engine.Instance.Spec.EngineState = litmuschaosv1alpha1.EngineStateActive
	}
	if engine.Instance.Spec.EngineState == litmuschaosv1alpha1.EngineStateActive && engine.Instance.Status.EngineStatus == "" {
		engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusInitialized
	}
	if engine.Instance.Status.EngineStatus == litmuschaosv1alpha1.EngineStatusInitialized {
		if engine.Instance.ObjectMeta.Finalizers == nil {
			engine.Instance.ObjectMeta.Finalizers = append(engine.Instance.ObjectMeta.Finalizers, finalizer)
			r.recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineInitialized", "%s created successfully", engine.Instance.Name+"-runner")
			if err := r.client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileForCreationAndRunning reconciles for Chaos execution of Chaos Engine
func (r *ReconcileChaosEngine) reconcileForCreationAndRunning(engine *chaosTypes.EngineInfo, reqLogger logr.Logger) (reconcile.Result, error) {

	err := r.validateAnnontatedApplication(engine)
	if err != nil {
		return reconcile.Result{}, err
	}

	//Check if the engineRunner pod already exists, else create
	err = r.checkEngineRunnerPod(engine, reqLogger)
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to get chaos resources")
		return reconcile.Result{}, err
	}

	isCompleted := r.checkRunnerPodCompletedStatus(engine)
	if isCompleted {
		err := r.updateEngineForComplete(engine, isCompleted)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// updateExperimentStatusesForStop updates ChaosEngine.Status.Experiment with Abort Status.
func updateExperimentStatusesForStop(engine *chaosTypes.EngineInfo) {
	for i := range engine.Instance.Status.Experiments {
		if engine.Instance.Status.Experiments[i].Status == litmuschaosv1alpha1.ExperimentStatusRunning || engine.Instance.Status.Experiments[i].Status == litmuschaosv1alpha1.ExperimentStatusWaiting {
			engine.Instance.Status.Experiments[i].Status = litmuschaosv1alpha1.ExperimentStatusAborted
			engine.Instance.Status.Experiments[i].Verdict = "Stopped"
			engine.Instance.Status.Experiments[i].LastUpdateTime = metav1.Now()
		}
	}
}

func startReqLogger(request reconcile.Request) logr.Logger {
	reqLogger := chaosTypes.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ChaosEngine")
	return reqLogger
}

func (r *ReconcileChaosEngine) validateAnnontatedApplication(engine *chaosTypes.EngineInfo) error {
	// Get the image for runner pod from chaosengine spec,operator env or default values.
	setChaosResourceImage()

	//getAnnotationCheck fetch the annotationCheck from engine spec
	err := getAnnotationCheck()
	if err != nil {
		return err
	}

	// Fetch the app details from ChaosEngine instance. Check if app is present
	// Also check, if the app is annotated for chaos & that the labels are unique
	err = getApplicationDetail(engine)
	if err != nil {
		r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to update chaosengine")
		return err
	}

	if engine.Instance.Spec.AnnotationCheck == "true" {
		// Determine whether apps with matching labels have chaos annotation set to true
		engine, err = resource.CheckChaosAnnotation(engine)
		if err != nil {
			r.recorder.Eventf(engine.Instance, corev1.EventTypeWarning, "ChaosResourcesOperationFailed", "Unable to get chaosengine")
			chaosTypes.Log.Info("Annotation check failed with", "error:", err)
			return nil
		}
	}
	return nil
}

func (r *ReconcileChaosEngine) updateEngineForComplete(engine *chaosTypes.EngineInfo, isCompleted bool) error {
	if engine.Instance.Status.EngineStatus != litmuschaosv1alpha1.EngineStatusCompleted {
		engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusCompleted
		engine.Instance.Spec.EngineState = litmuschaosv1alpha1.EngineStateStop
		if err := r.client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
			return err
		}
		r.recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "ChaosEngineCompleted", "Chaos Engine completed, will delete or retain the resources according to jobCleanUpPolicy")
	}
	return nil
}

func (r *ReconcileChaosEngine) updateEngineForRestart(engine *chaosTypes.EngineInfo) error {
	r.recorder.Eventf(engine.Instance, corev1.EventTypeNormal, "RestartInProgress", "Chaos Engine restarted, will re-create all chaos-resources")
	engine.Instance.Status.EngineStatus = litmuschaosv1alpha1.EngineStatusInitialized
	engine.Instance.Status.Experiments = nil
	if err := r.client.Update(context.TODO(), engine.Instance, &client.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (r *ReconcileChaosEngine) forceRemoveChaosResources(engine *chaosTypes.EngineInfo, request reconcile.Request) error {
	err := r.forceRemoveAllChaosPods(engine, request)
	if err != nil {
		return err
	}
	return nil
}
