/*
Copyright 2019 The Rook Authors. All rights reserved.

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

package k8sutil

import (
	"fmt"
	"time"

	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/util"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// make a headless svc for statefulset
func makeHeadlessSvc(name, namespace string, label map[string]string, clientset kubernetes.Interface) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    label,
		},
		Spec: corev1.ServiceSpec{
			Selector: label,
			Ports:    []corev1.ServicePort{{Name: "dummy", Port: 1234}},
		},
	}

	_, err := clientset.CoreV1().Services(namespace).Create(svc)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create %s headless service. %+v", name, err)
	}
	return svc, nil
}

// create a apps.statefulset and a headless svc
func CreateStatefulSet(name, namespace, appName string, clientset kubernetes.Interface, ss *apps.StatefulSet) (*corev1.Service, error) {
	label := ss.GetLabels()
	svc, err := makeHeadlessSvc(appName, namespace, label, clientset)
	if err != nil {
		return nil, fmt.Errorf("failed to start %s service: %v\n%v", name, err, ss)
	}

	_, err = clientset.AppsV1().StatefulSets(namespace).Create(ss)
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			_, err = clientset.AppsV1().StatefulSets(namespace).Update(ss)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to start %s statefulset: %v\n%v", name, err, ss)
		}
	}
	return svc, err
}

// UpdateStatefulSetAndWait updates a statefulset and waits until it is
// running to return. It will error if the statefulset does not exist to
// be updated or if it takes too long.
//
//   Note: it is required that the statefulset uses the RollingUpdate
//         update strategy. this will not work as expected with OnDelete
//
// This method has a generic callback function that each backend can
// rely on. It serves two purposes:
//
//   1. verify that a resource can be stopped
//   2. verify that we can continue the update procedure
//
func UpdateStatefulSetAndWait(
	context *clusterd.Context,
	sts *apps.StatefulSet,
	namespace string,
	verifyCallback func(action string) error,
) (*apps.StatefulSet, error) {

	original, err := context.Clientset.AppsV1().StatefulSets(namespace).Get(
		sts.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get statefulset %s. %+v", sts.Name, err)
	}

	if original.Spec.UpdateStrategy.Type != apps.RollingUpdateStatefulSetStrategyType {
		return nil, fmt.Errorf("can only update statefulsets with rolling updates")
	}

	// check if the stateful can be stopped for the update
	err = util.Retry(5, 60*time.Second, func() error {
		return verifyCallback("stop")
	})
	if err != nil {
		return nil, fmt.Errorf("failed to check if statefulset %s can be "+
			"updated: %+v", sts.Name, err)
	}

	// perform the update
	logger.Infof("updating statefulset %s", sts.Name)
	_, err = context.Clientset.AppsV1().StatefulSets(namespace).Update(sts)
	if err != nil {
		return nil, fmt.Errorf("failed to update statefulset %s. %+v",
			sts.Name, err)
	}

	// wait for update to complete
	sleepTime := 2
	attempts := 30
	for i := 0; i < attempts; i++ {
		latest, err := context.Clientset.AppsV1().StatefulSets(namespace).Get(
			sts.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get statefulset %s. %+v", sts.Name, err)
		}

		// we're done once we detect the new statefulset and that it's running
		// pods created from the updated spec
		if latest.Status.ObservedGeneration != original.Status.ObservedGeneration &&
			latest.Status.UpdatedReplicas > 0 && latest.Status.ReadyReplicas > 0 {

			logger.Infof("finished waiting for updated statefulset %s", sts.Name)

			err = verifyCallback("continue")
			if err != nil {
				return nil, fmt.Errorf("failed to check if statefulset %s can "+
					"be updated: %+v", sts.Name, err)
			}

			return sts, nil
		}

		logger.Debugf("statefulset %s status=%+v", sts.Name, sts.Status)
		time.Sleep(time.Duration(sleepTime) * time.Second)
	}

	return nil, fmt.Errorf("gave up waiting for statefulset %s to update", sts.Name)
}
