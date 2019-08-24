/*
Copyright 2018 The Rook Authors. All rights reserved.

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

package mon

import (
	"fmt"
	"time"

	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	cephconfig "github.com/rook/rook/pkg/daemon/ceph/config"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	// HealthCheckInterval is the interval to check if the mons are in quorum
	HealthCheckInterval = 45 * time.Second
	// MonOutTimeout is the duration to wait before removing/failover to a new mon pod
	MonOutTimeout = 600 * time.Second
)

// HealthChecker aggregates the mon/cluster info needed to check the health of the monitors
type HealthChecker struct {
	monCluster *Cluster
}

// NewHealthChecker creates a new HealthChecker object
func NewHealthChecker(monCluster *Cluster) *HealthChecker {
	return &HealthChecker{
		monCluster: monCluster,
	}
}

// Check periodically checks the health of the monitors
func (hc *HealthChecker) Check(stopCh chan struct{}) {
	for {
		select {
		case <-stopCh:
			logger.Infof("Stopping monitoring of mons in namespace %s", hc.monCluster.Namespace)
			return

		case <-time.After(HealthCheckInterval):
			logger.Debugf("checking health of mons")
			err := hc.monCluster.checkHealth()
			if err != nil {
				logger.Infof("failed to check mon health. %+v", err)
			}
		}
	}
}

func (c *Cluster) checkHealth() error {
	c.acquireOrchestrationLock()
	defer c.releaseOrchestrationLock()

	logger.Debugf("Checking health for mons in cluster. %s", c.clusterInfo.Name)

	// Use a local mon count in case the user updates the crd in another goroutine.
	// We need to complete a health check with a consistent value.
	desiredMonCount := c.spec.Mon.Count
	logger.Debugf("targeting the mon count %d", desiredMonCount)

	// connect to the mons
	// get the status and check for quorum
	status, err := client.GetMonStatus(c.context, c.clusterInfo.Name, true)
	if err != nil {
		return fmt.Errorf("failed to get mon status. %+v", err)
	}
	logger.Debugf("Mon status: %+v", status)

	// Source of truth of which mons should exist is our *clusterInfo*
	monsNotFound := map[string]interface{}{}
	for _, mon := range c.clusterInfo.Monitors {
		monsNotFound[mon.Name] = struct{}{}
	}

	// first handle mons that are not in quorum but in the ceph mon map
	// failover the unhealthy mons
	allMonsInQuorum := true
	for _, mon := range status.MonMap.Mons {
		inQuorum := monInQuorum(mon, status.Quorum)
		// if the mon is in quorum remove it from our check for "existence"
		// else see below condition
		if _, ok := monsNotFound[mon.Name]; ok {
			delete(monsNotFound, mon.Name)
		} else {
			// when the mon isn't in the clusterInfo, but is in quorum and there are
			// enough mons, remove it else remove it on the next run
			if inQuorum && len(status.MonMap.Mons) > desiredMonCount {
				logger.Warningf("mon %s not in source of truth but in quorum, removing", mon.Name)
				c.removeMon(mon.Name)
			} else {
				logger.Warningf(
					"mon %s not in source of truth and not in quorum, not enough mons to remove now (wanted: %d, current: %d)",
					mon.Name,
					desiredMonCount,
					len(status.MonMap.Mons),
				)
			}
		}

		if inQuorum {
			logger.Debugf("mon %s found in quorum", mon.Name)
			// delete the "timeout" for a mon if the pod is in quorum again
			if _, ok := c.monTimeoutList[mon.Name]; ok {
				delete(c.monTimeoutList, mon.Name)
				logger.Infof("mon %s is back in quorum, removed from mon out timeout list", mon.Name)
			}
		} else {
			logger.Debugf("mon %s NOT found in quorum. Mon status: %+v", mon.Name, status)
			allMonsInQuorum = false

			// If not yet set, add the current time, for the timeout
			// calculation, to the list
			if _, ok := c.monTimeoutList[mon.Name]; !ok {
				c.monTimeoutList[mon.Name] = time.Now()
			}

			// when the timeout for the mon has been reached, continue to the
			// normal failover/delete mon pod part of the code
			if time.Since(c.monTimeoutList[mon.Name]) <= MonOutTimeout {
				logger.Warningf("mon %s not found in quorum, waiting for timeout before failover", mon.Name)
				continue
			}

			logger.Warningf("mon %s NOT found in quorum and timeout exceeded, mon will be failed over", mon.Name)
			c.failMon(len(status.MonMap.Mons), desiredMonCount, mon.Name)
			// only deal with one unhealthy mon per health check
			return nil
		}
	}

	// after all unhealthy mons have been removed/failovered
	// handle all mons that haven't been in the Ceph mon map
	for mon := range monsNotFound {
		logger.Warningf("mon %s NOT found in ceph mon map, failover", mon)
		c.failMon(len(c.clusterInfo.Monitors), desiredMonCount, mon)
		// only deal with one "not found in ceph mon map" mon per health check
		return nil
	}

	// create/start new mons when there are fewer mons than the desired count in the CRD
	if len(status.MonMap.Mons) < desiredMonCount {
		logger.Infof("adding mons. currently %d mons are in quorum and the desired count is %d.", len(status.MonMap.Mons), desiredMonCount)
		return c.startMons(desiredMonCount)
	}

	// remove extra mons if the desired count has decreased in the CRD and all the mons are currently healthy
	if allMonsInQuorum && len(status.MonMap.Mons) > desiredMonCount {
		if desiredMonCount < 2 && len(status.MonMap.Mons) == 2 {
			logger.Warningf("cannot reduce mon quorum size from 2 to 1")
		} else {
			logger.Infof("removing an extra mon. currently %d are in quorum and only %d are desired", len(status.MonMap.Mons), desiredMonCount)
			return c.removeMon(status.MonMap.Mons[0].Name)
		}
	}

	return nil
}

// failMon compares the monCount against desiredMonCount
func (c *Cluster) failMon(monCount, desiredMonCount int, name string) {
	if monCount > desiredMonCount {
		// no need to create a new mon since we have an extra
		if err := c.removeMon(name); err != nil {
			logger.Errorf("failed to remove mon %s. %+v", name, err)
		}
	} else {
		// bring up a new mon to replace the unhealthy mon
		if err := c.failoverMon(name); err != nil {
			logger.Errorf("failed to failover mon %s. %+v", name, err)
		}
	}
}

func (c *Cluster) failoverMon(name string) error {
	logger.Infof("Failing over monitor %s", name)

	// Start a new monitor
	m := c.newMonConfig(c.maxMonID + 1)
	logger.Infof("starting new mon: %+v", m)

	mConf := []*monConfig{m}

	// Assign the pod to a node
	if err := c.assignMons(mConf); err != nil {
		return fmt.Errorf("failed to place new mon on a node. %+v", err)
	}

	if c.HostNetwork {
		node, ok := c.mapping.Node[m.DaemonName]
		if !ok {
			return fmt.Errorf("mon %s doesn't exist in assignment map", m.DaemonName)
		}
		m.PublicIP = node.Address
	} else {
		// Create the service endpoint
		serviceIP, err := c.createService(m)
		if err != nil {
			return fmt.Errorf("failed to create mon service. %+v", err)
		}
		m.PublicIP = serviceIP
	}
	c.clusterInfo.Monitors[m.DaemonName] = cephconfig.NewMonInfo(m.DaemonName, m.PublicIP, m.Port)

	// Start the deployment
	if err := c.startDeployments(mConf, true); err != nil {
		return fmt.Errorf("failed to start new mon %s. %+v", m.DaemonName, err)
	}

	// Only increment the max mon id if the new pod started successfully
	c.maxMonID++

	return c.removeMon(name)
}

func (c *Cluster) removeMon(daemonName string) error {
	logger.Infof("ensuring removal of unhealthy monitor %s", daemonName)

	resourceName := resourceName(daemonName)

	// Remove the mon pod if it is still there
	var gracePeriod int64
	propagation := metav1.DeletePropagationForeground
	options := &metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod, PropagationPolicy: &propagation}
	if err := c.context.Clientset.AppsV1().Deployments(c.Namespace).Delete(resourceName, options); err != nil {
		if errors.IsNotFound(err) {
			logger.Infof("dead mon %s was already gone", resourceName)
		} else {
			return fmt.Errorf("failed to remove dead mon deployment %s. %+v", resourceName, err)
		}
	}

	// Remove the bad monitor from quorum
	if err := removeMonitorFromQuorum(c.context, c.clusterInfo.Name, daemonName); err != nil {
		return fmt.Errorf("failed to remove mon %s from quorum. %+v", daemonName, err)
	}
	delete(c.clusterInfo.Monitors, daemonName)
	// check if a mapping exists for the mon
	if _, ok := c.mapping.Node[daemonName]; ok {
		delete(c.mapping.Node, daemonName)
	}

	// Remove the service endpoint
	if err := c.context.Clientset.CoreV1().Services(c.Namespace).Delete(resourceName, options); err != nil {
		if errors.IsNotFound(err) {
			logger.Infof("dead mon service %s was already gone", resourceName)
		} else {
			return fmt.Errorf("failed to remove dead mon service %s. %+v", resourceName, err)
		}
	}

	if err := c.saveMonConfig(); err != nil {
		return fmt.Errorf("failed to save mon config after failing over mon %s. %+v", daemonName, err)
	}

	// make sure to rewrite the config so NO new connections are made to the removed mon
	if err := WriteConnectionConfig(c.context, c.clusterInfo); err != nil {
		return fmt.Errorf("failed to write connection config after failing over mon %s. %+v", daemonName, err)
	}

	return nil
}

func removeMonitorFromQuorum(context *clusterd.Context, clusterName, name string) error {
	logger.Debugf("removing monitor %s", name)
	args := []string{"mon", "remove", name}
	if _, err := client.NewCephCommand(context, clusterName, args).Run(); err != nil {
		return fmt.Errorf("mon %s remove failed: %+v", name, err)
	}

	logger.Infof("removed monitor %s", name)
	return nil
}
