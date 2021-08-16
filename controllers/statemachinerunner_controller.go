/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"github.com/ghodss/yaml"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	eventingapi "knative.dev/eventing/pkg/apis/eventing/v1"
	knativeEventingClient "knative.dev/eventing/pkg/client/clientset/versioned"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	servingapi "knative.dev/serving/pkg/apis/serving/v1"
	knativeServingClient "knative.dev/serving/pkg/client/clientset/versioned"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	statev1 "github.com/salaboy/knative-state/api/v1"
)

var RUNNER_IMAGE = os.Getenv("RUNNER_IMAGE")

// StateMachineRunnerReconciler reconciles a WorkflowRunner object
type StateMachineRunnerReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	knativeServingClient  *knativeServingClient.Clientset
	knativeEventingClient *knativeEventingClient.Clientset
}

//+kubebuilder:rbac:groups=flow.knative.dev,resources=statemachinerunners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=flow.knative.dev,resources=statemachinerunners/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=flow.knative.dev,resources=statemachinerunners/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the WorkflowRunner object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile

// Reconciling a new WorkflowRunner should:
// - Check if the WorkflowRef exists, if not fail and do nothing
// - If the WorkflowRef exists, fetch it and then:
//   - Check if there is no StateMachineRunner for the pair (workflow + version)
//      - if not:
//        - Create the Runner Knative Service with the name specificed in the runner, the name should include the version
//          - Create a dedicated broker for the runner
//      - if yes:
//        - do nothing, but fetch the broker
//   	- Create Triggers for Events based on the StateMachineRef on the dedicated broker
//

func (r *StateMachineRunnerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var stateMachineRunner statev1.StateMachineRunner

	if err := r.Get(ctx, req.NamespacedName, &stateMachineRunner); err != nil {
		// it might be not found if this is a delete request
		if ignoreNotFound(err) == nil {
			log.Info("Hey there.. deleting workflowrunner happened: " + req.NamespacedName.Name)

			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch workflowrunner")

		return ctrl.Result{}, err
	}

	if stateMachineRunner.Spec.StateMachineRef != "" {
		var stateMachine statev1.StateMachine
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: "default",
			Name:      stateMachineRunner.Spec.StateMachineRef,
		}, &stateMachine); err != nil {
			// it might be not found if this is a delete request

			return ctrl.Result{}, err
		}

		yamlStates, err := yaml.Marshal(stateMachine.Spec.StateMachineDefinition.StateMachineStates.States)
		if err != nil {
			log.Error(err, "failed to parse yaml from statemachine definition states")
			return ctrl.Result{}, err
		}
		if RUNNER_IMAGE == "" {
			RUNNER_IMAGE = "kind.local/knative-statemachine-runner-7a3c815d2bf3ebf9af9650f7624a29c9:93b9adcd6af50be3ba7f7b4848c79da214c0b4dcca39709c98c28905eb91b6a0"
		}
		service := &servingapi.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kservice-" + stateMachine.Name,
				Namespace: "default",
			},
			Spec: servingapi.ServiceSpec{
				ConfigurationSpec: servingapi.ConfigurationSpec{

					Template: servingapi.RevisionTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								// Remove if we have redis in the runner
								"autoscaling.knative.dev/minScale": "1",
							},
						},
						Spec: servingapi.RevisionSpec{
							PodSpec: v1.PodSpec{

								Containers: []v1.Container{
									v1.Container{
										Name:  "knative-statemachine-runner",
										Image: RUNNER_IMAGE,
										Env: []v1.EnvVar{
											v1.EnvVar{
												Name:  "STATEMACHINE_NAME",
												Value: stateMachine.Name,
											},
											v1.EnvVar{
												Name:  "STATEMACHINE_VERSION",
												Value: stateMachine.Spec.StateMachineDefinition.Version,
											},
											v1.EnvVar{
												Name:  "STATEMACHINE_DEF",
												Value: fmt.Sprintf("%s", yamlStates),
											},
											v1.EnvVar{
												Name:  "EVENT_SINK",
												Value: stateMachineRunner.Spec.Sink,
											},
											v1.EnvVar{
												Name:  "REDIS_HOST",
												Value: stateMachineRunner.Spec.RedisHost,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		serviceExist, err := r.knativeServingClient.ServingV1().Services("default").Get(ctx, service.Name, metav1.GetOptions{})
		if err != nil {
			if ignoreNotFound(err) == nil {
				log.Info("KService doesn't exist, so creating KService: " + service.Name)
				_, err := ctrl.CreateOrUpdate(ctx, r.Client, service, func() error {
					return ctrl.SetControllerReference(&stateMachineRunner, service, r.Scheme)
				})
				if err != nil {
					log.Error(err, "Error Creating or Updating and Setting Controller References to Knative Service: "+service.Name)
				}
			} else {
				log.Error(err, "Error Fetching Knative Service: "+service.Name)
			}
		} else if serviceExist.Name != "" {
			log.Info("KService exist, so checking the Status URL: " + service.Name)
			if serviceExist.Status.URL != nil {

				log.Info("> Created KService URL for subscriber : " + serviceExist.Status.URL.String())
				parsedURL, err := apis.ParseURL("http://" + serviceExist.Name + ".default.svc.cluster.local" + "/statemachines/events")
				if err != nil {
					log.Error(err, "Error Parsing URl for: "+serviceExist.Status.URL.String())
					return ctrl.Result{}, err
				}

				//@TODO: create broker
				// Maybe Using name from: workflowRunner.Spec.Broker, if not specified use the workflow name and version
				//

				// Create Triggers for Workflow definition
				for stateType, _ := range stateMachine.Spec.StateMachineDefinition.StateMachineStates.States {
					log.Info("> Looking for Events in State : " + string(stateType))
					// Create triggers for events that the workflow is waiting for
					for eventName, _ := range stateMachine.Spec.StateMachineDefinition.StateMachineStates.States[stateType].Events {
						log.Info("> Creating trigger for Event: " + string(eventName) + " in State : " + string(stateType))
						trigger := &eventingapi.Trigger{
							ObjectMeta: metav1.ObjectMeta{
								Name:      strings.ToLower("t-" + stateMachine.Name + "-" + string(eventName)),
								Namespace: "default",
							},
							Spec: eventingapi.TriggerSpec{
								Broker: stateMachineRunner.Spec.Broker,
								Filter: &eventingapi.TriggerFilter{
									Attributes: map[string]string{
										"type": string(eventName),
									},
								},
								Subscriber: duckv1.Destination{
									URI: parsedURL,
								},
							},
						}
						_, err := ctrl.CreateOrUpdate(ctx, r.Client, trigger, func() error {
							return ctrl.SetControllerReference(&stateMachineRunner, trigger, r.Scheme)
						})
						if err != nil {
							log.Error(err, "Error Creating or Updating and Setting Controller References to Knative Trigger: "+trigger.Name)
						}

					}
				}

				for _, condition := range serviceExist.Status.Conditions {
					if condition.Type == apis.ConditionReady {
						log.Info("Runner Ready! ")
						stateMachineRunner.Status.RunnerUrl = "http://" + serviceExist.Name + ".default.127.0.0.1.nip.io"
						stateMachineRunner.Status.RunnerId = ""  // Need to fetch the ID from the Info endpoint
						stateMachineRunner.Status.BrokerUrl = "" // Need to check if the broker is up and add the URL here
						if err := r.Status().Update(ctx, &stateMachineRunner); err != nil {
							log.Error(err, "unable to update StateMachineRunner status")
							return ctrl.Result{}, err
						}
					}
				}

			} else {
				log.Info("KService exist, but Status URL is nil")
			}
		}

	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *StateMachineRunnerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.knativeServingClient = knativeServingClient.NewForConfigOrDie(mgr.GetConfig())
	r.knativeEventingClient = knativeEventingClient.NewForConfigOrDie(mgr.GetConfig())
	return ctrl.NewControllerManagedBy(mgr).
		For(&statev1.StateMachineRunner{}).
		Owns(&servingapi.Service{}).
		Owns(&eventingapi.Trigger{}).
		Complete(r)
}

func ignoreNotFound(err error) error {
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
