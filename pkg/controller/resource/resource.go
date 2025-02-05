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

package resource

import (
	"fmt"
	"os"
	"strings"

	chaosTypes "github.com/litmuschaos/chaos-operator/pkg/controller/types"
	k8s "github.com/litmuschaos/chaos-operator/pkg/kubernetes"
)

// Annotations on app to enable chaos on it
const (
	ChaosAnnotationValue      = "true"
	DefaultChaosAnnotationKey = "litmuschaos.io/chaos"
)

var (
	// ChaosAnnotationKey is global variable used as the Key for annotation check.
	ChaosAnnotationKey = getAnnotationKey()
)

// getAnnotationKey returns the annotation to be used while validating applications.
func getAnnotationKey() string {

	annotationKey := os.Getenv("CUSTOM_ANNOTATION")
	if len(annotationKey) != 0 {
		return annotationKey
	}
	return DefaultChaosAnnotationKey

}

// CheckChaosAnnotation will check for the annotation of required resources
func CheckChaosAnnotation(engine *chaosTypes.EngineInfo) (*chaosTypes.EngineInfo, error) {
	// Use client-Go to obtain a list of apps w/ specified labels
	//var chaosEngine chaosTypes.EngineInfo
	clientSet, err := k8s.CreateClientSet()
	if err != nil {
		return engine, fmt.Errorf("clientset generation failed with error: %+v", err)
	}
	switch strings.ToLower(engine.AppInfo.Kind) {
	case "deployment", "deployments":
		engine, err = CheckDeploymentAnnotation(clientSet, engine)
		if err != nil {
			return engine, fmt.Errorf("resource type 'deployment', err: %+v", err)
		}
	case "statefulset", "statefulsets":
		engine, err = CheckStatefulSetAnnotation(clientSet, engine)
		if err != nil {
			return engine, fmt.Errorf("resource type 'statefulset', err: %+v", err)
		}
	case "daemonset", "daemonsets":
		engine, err = CheckDaemonSetAnnotation(clientSet, engine)
		if err != nil {
			return engine, fmt.Errorf("resource type 'daemonset', err: %+v", err)
		}
	default:
		return engine, fmt.Errorf("resource type '%s' not supported for induce chaos", engine.AppInfo.Kind)
	}
	chaosTypes.Log.Info("chaos candidate of", "kind:", engine.AppInfo.Kind, "appName: ", engine.AppName, "appUUID: ", engine.AppUUID)
	return engine, nil
}

// CountTotalChaosEnabled will count the number of chaos enabled applications
func CountTotalChaosEnabled(annotationValue string, chaosCandidates int) int {
	if annotationValue == ChaosAnnotationValue {
		chaosCandidates++
	}
	return chaosCandidates
}
