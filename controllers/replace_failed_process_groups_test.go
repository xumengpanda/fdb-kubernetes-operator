/*
 * replace_failed_process_groups_test.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2021 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	ctx "context"
	"fmt"
	"time"

	"github.com/FoundationDB/fdb-kubernetes-operator/pkg/fdbadminclient/mock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	"github.com/FoundationDB/fdb-kubernetes-operator/internal"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("replace_failed_process_groups", func() {
	var cluster *fdbv1beta2.FoundationDBCluster
	var err error
	var result *requeue

	BeforeEach(func() {
		cluster = internal.CreateDefaultCluster()
		err = k8sClient.Create(ctx.TODO(), cluster)
		Expect(err).NotTo(HaveOccurred())

		result, err := reconcileCluster(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeFalse())

		// Q: why do we need to reloadCluster? Check there is only 1 recovery?
		generation, err := reloadCluster(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(generation).To(Equal(int64(1)))
	})

	// TODO: Test replace for taint info
	JustBeforeEach(func() {
		fmt.Printf("-----MX Test Replace Tainted Pod START JustBeforeEach---------\n")
		adminClient, err := mock.NewMockAdminClientUncast(cluster, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(adminClient).NotTo(BeNil())
		err = internal.NormalizeClusterSpec(cluster, internal.DeprecationOptions{})
		Expect(err).NotTo(HaveOccurred())
		result = replaceFailedProcessGroups{}.reconcile(ctx.Background(), clusterReconciler, cluster)
	})

	FContext("replace pod on tainted node", func() {
		taintKeyStar := "*"
		taintKeyStarDuration := int64(20)
		taintKeyMaintenance := "foundationdb/maintenance"
		taintKeyMaintenanceDuration := int64(5)
		var allPods []*corev1.Pod
		var allPvcs *corev1.PersistentVolumeClaimList
		var taint bool
		var pod *corev1.Pod                             // Pod to be tainted
		var podProcessGroupID fdbv1beta2.ProcessGroupID // Target pod's process group id
		var node *corev1.Node
		var adminClient *mock.AdminClient
		var configMap *corev1.ConfigMap
		var processMap map[fdbv1beta2.ProcessGroupID][]fdbv1beta2.FoundationDBStatusProcessInfo

		BeforeEach(func() {
			fmt.Printf("-----MX Test Replace Tainted Pod START---------\n")
			cluster.Spec.AutomationOptions.Replacements.TaintReplacementOptions = []fdbv1beta2.TaintReplacementOption{
				{
					Key:               &taintKeyStar,
					DurationInSeconds: &taintKeyStarDuration,
				},
				{
					Key:               &taintKeyMaintenance,
					DurationInSeconds: &taintKeyMaintenanceDuration,
				},
			}

			adminClient, err = mock.NewMockAdminClientUncast(cluster, k8sClient)

			Expect(err).NotTo(HaveOccurred())
			databaseStatus, err := adminClient.GetStatus()
			Expect(err).NotTo(HaveOccurred())
			processMap = make(map[fdbv1beta2.ProcessGroupID][]fdbv1beta2.FoundationDBStatusProcessInfo)
			for _, process := range databaseStatus.Cluster.Processes {
				processID, ok := process.Locality["process_id"]
				// if the processID is not set we fall back to the instanceID
				if !ok {
					processID = process.Locality["instance_id"]
				}
				processMap[fdbv1beta2.ProcessGroupID(processID)] = append(processMap[fdbv1beta2.ProcessGroupID(processID)], process)
			}

			allPods, err = clusterReconciler.PodLifecycleManager.GetPods(context.TODO(), clusterReconciler, cluster, internal.GetPodListOptions(cluster, "", "")...)
			Expect(err).NotTo(HaveOccurred())

			allPvcs = &corev1.PersistentVolumeClaimList{}
			err = clusterReconciler.List(context.TODO(), allPvcs, internal.GetPodListOptions(cluster, "", "")...)
			Expect(err).NotTo(HaveOccurred())

			configMap = &corev1.ConfigMap{}
			err = k8sClient.Get(context.TODO(), types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-config"}, configMap)
			Expect(err).NotTo(HaveOccurred())

			pods, err := clusterReconciler.PodLifecycleManager.GetPods(context.TODO(), clusterReconciler, cluster, internal.GetSinglePodListOptions(cluster, "storage-1")...)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(pods)).To(Equal(1))

			pod = pods[0] // TODO: choose a random pod
			node = &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: pod.Spec.NodeName},
			}
			podProcessGroupID = internal.GetProcessGroupIDFromMeta(cluster, pod.ObjectMeta)
			fmt.Printf("Testing ProcessGroupID: %+v\n", podProcessGroupID)
			taint = false

			// Call validateProcessGroups to set processGroupStatus to tainted condition
			processGroupsStatus, err := validateProcessGroups(context.TODO(), clusterReconciler, cluster, &cluster.Status, processMap, configMap, allPods, allPvcs)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(processGroupsStatus)).To(BeNumerically(">", 4))
			processGroup := processGroupsStatus[len(processGroupsStatus)-4]
			Expect(processGroup.ProcessGroupID).To(Equal(fdbv1beta2.ProcessGroupID("storage-1")))
			Expect(len(processGroupsStatus[0].ProcessGroupConditions)).To(Equal(0))
		})

		PIt("should not replace a pod that is NodeTaintDetected but not NodeTaintReplacing ", func() {
			taint = true
			if taint {
				node.Spec.Taints = []corev1.Taint{
					{
						Key:       taintKeyMaintenance,
						Value:     "rack maintenance",
						Effect:    corev1.TaintEffectNoExecute,
						TimeAdded: &metav1.Time{Time: time.Now()},
					},
				}
				log.Info("Taint node", "Node name", pod.Name, "Node taints", node.Spec.Taints)
				//fmt.Printf("Create tainted node:%s\n", node.Name)
				// Make taint in effect
				err = k8sClient.Update(context.TODO(), node)
				Expect(err).NotTo(HaveOccurred())
			}
			processGroupsStatus, err := validateProcessGroups(context.TODO(), clusterReconciler, cluster, &cluster.Status, processMap, configMap, allPods, allPvcs)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(processGroupsStatus)).To(BeNumerically(">", 4))
			targetProcessGroupStatus := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, podProcessGroupID)
			Expect(len(targetProcessGroupStatus.ProcessGroupConditions)).To(Equal(1))
			Expect(targetProcessGroupStatus.ProcessGroupConditions[0].ProcessGroupConditionType).To(Equal(fdbv1beta2.NodeTaintDetected))

			result = replaceFailedProcessGroups{}.reconcile(ctx.TODO(), clusterReconciler, cluster)
			Expect(result).To(BeNil())
			Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
			fmt.Printf("Test Debug Info: RemovedProcessGroupIDs %+v, TargetProcessGroupID:%+v\n", getRemovedProcessGroupIDs(cluster), nil)
			fmt.Printf("-----MX Test Replace Tainted Pod FINISH---------\n")
		})

		It("should replace a pod that is both NodeTaintDetected and NodeTaintReplacing ", func() {
			taint = true
			if taint {
				node.Spec.Taints = []corev1.Taint{
					{
						Key:       taintKeyMaintenance,
						Value:     "rack maintenance",
						Effect:    corev1.TaintEffectNoExecute,
						TimeAdded: &metav1.Time{Time: time.Now().Add(-time.Second * time.Duration(taintKeyMaintenanceDuration+1))},
					},
				}
				log.Info("Taint node", "Node name", pod.Name, "Node taints", node.Spec.Taints, "TaintTime", node.Spec.Taints[0].TimeAdded.Time, "Now", time.Now())
				//fmt.Printf("Create tainted node:%s\n", node.Name)
				// Make taint in effect
				err = k8sClient.Update(context.TODO(), node)
				Expect(err).NotTo(HaveOccurred())
			}
			processGroupsStatus, err := validateProcessGroups(context.TODO(), clusterReconciler, cluster, &cluster.Status, processMap, configMap, allPods, allPvcs)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(processGroupsStatus)).To(BeNumerically(">", 4))
			targetProcessGroupStatus := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, podProcessGroupID)
			Expect(len(targetProcessGroupStatus.ProcessGroupConditions)).To(Equal(2))
			Expect(targetProcessGroupStatus.GetCondition(fdbv1beta2.NodeTaintDetected).ProcessGroupConditionType).To(Equal(fdbv1beta2.NodeTaintDetected))
			Expect(targetProcessGroupStatus.GetCondition(fdbv1beta2.NodeTaintReplacing).ProcessGroupConditionType).To(Equal(fdbv1beta2.NodeTaintReplacing))

			// cluster won't replace a failed process until GetFailureDetectionTimeSeconds() later
			time.Sleep(time.Second * time.Duration(cluster.GetFailureDetectionTimeSeconds()+1))
			result = replaceFailedProcessGroups{}.reconcile(ctx.TODO(), clusterReconciler, cluster)
			Expect(result).NotTo(BeNil())
			Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{podProcessGroupID}))
			fmt.Printf("Test Debug Info: RemovedProcessGroupIDs %+v, TargetProcessGroupID:%+v\n", getRemovedProcessGroupIDs(cluster), podProcessGroupID)
			fmt.Printf("-----MX Test Replace Tainted Pod FINISH---------\n")
		})

		// PIt("should replace the pod that has NodeTaintReplacing condition", func() {
		// 	taint = true
		// 	if taint {
		// 		node.Spec.Taints = []corev1.Taint{
		// 			{
		// 				Key:       "foundationdb/maintenance",
		// 				Value:     "rack maintenance",
		// 				Effect:    corev1.TaintEffectNoExecute,
		// 				TimeAdded: &metav1.Time{Time: time.Now().Add(-time.Second * time.Duration(taintKeyMaintenanceDuration))},
		// 			},
		// 		}
		// 		log.Info("Taint node", "Node name", pod.Name, "Node taints", node.Spec.Taints)
		// 		//fmt.Printf("Create tainted node:%s\n", node.Name)
		// 		// Make taint in effect
		// 		err = k8sClient.Update(ctx.TODO(), node)
		// 		Expect(err).NotTo(HaveOccurred())
		// 	}
		// 	result = replaceFailedProcessGroups{}.reconcile(ctx.TODO(), clusterReconciler, cluster)
		// 	Expect(result).NotTo(BeNil())
		// 	Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{podProcessGroupID}))
		// 	fmt.Printf("RemovedProcessGroupIDs %+v, TargetProcessGroupID:%+v\n", getRemovedProcessGroupIDs(cluster), podProcessGroupID)
		// 	fmt.Printf("-----MX Test Replace Tainted Pod FINISH---------\n")
		// })
	})

	Context("with no missing processes", func() {
		It("should return nil",
			func() {
				Expect(result).To(BeNil())
			})

		It("should not mark anything for removal", func() {
			Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
		})
	})

	Context("with a process that has been missing for a long time", func() {
		BeforeEach(func() {
			processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
			processGroup.ProcessGroupConditions = append(processGroup.ProcessGroupConditions, &fdbv1beta2.ProcessGroupCondition{
				ProcessGroupConditionType: fdbv1beta2.MissingProcesses,
				Timestamp:                 time.Now().Add(-1 * time.Hour).Unix(),
			})
		})

		Context("with no other removals", func() {
			It("should requeue", func() {
				Expect(result).NotTo(BeNil())
				Expect(result.message).To(Equal("Removals have been updated in the cluster status"))
			})

			It("should mark the process group for removal", func() {
				Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-2"}))
			})

			It("should not be marked to skip exclusion", func() {
				for _, pg := range cluster.Status.ProcessGroups {
					if pg.ProcessGroupID != "storage-2" {
						continue
					}

					Expect(pg.ExclusionSkipped).To(BeFalse())
				}
			})

			When("EmptyMonitorConf is set to true", func() {
				BeforeEach(func() {
					cluster.Spec.Buggify.EmptyMonitorConf = true
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})

			When("Crash loop is set for all process groups", func() {
				BeforeEach(func() {
					cluster.Spec.Buggify.CrashLoop = []fdbv1beta2.ProcessGroupID{"*"}
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})

			When("Crash loop is set for the specific process group", func() {
				BeforeEach(func() {
					cluster.Spec.Buggify.CrashLoop = []fdbv1beta2.ProcessGroupID{"storage-2"}
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})

			When("Crash loop is set for the main container", func() {
				BeforeEach(func() {
					cluster.Spec.Buggify.CrashLoopContainers = []fdbv1beta2.CrashLoopContainerObject{
						{
							ContainerName: fdbv1beta2.MainContainerName,
							Targets:       []fdbv1beta2.ProcessGroupID{"storage-2"},
						},
					}
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})

			When("Crash loop is set for the sidecar container", func() {
				BeforeEach(func() {
					cluster.Spec.Buggify.CrashLoopContainers = []fdbv1beta2.CrashLoopContainerObject{
						{
							ContainerName: fdbv1beta2.SidecarContainerName,
							Targets:       []fdbv1beta2.ProcessGroupID{"storage-2"},
						},
					}
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})
		})

		Context("with multiple failed processes", func() {
			BeforeEach(func() {
				processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-3")
				processGroup.ProcessGroupConditions = append(processGroup.ProcessGroupConditions, &fdbv1beta2.ProcessGroupCondition{
					ProcessGroupConditionType: fdbv1beta2.MissingProcesses,
					Timestamp:                 time.Now().Add(-1 * time.Hour).Unix(),
				})
			})

			It("should requeue", func() {
				Expect(result).NotTo(BeNil())
				Expect(result.message).To(Equal("Removals have been updated in the cluster status"))
			})

			It("should mark the first process group for removal", func() {
				Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-2"}))
			})

			It("should not be marked to skip exclusion", func() {
				for _, pg := range cluster.Status.ProcessGroups {
					if pg.ProcessGroupID != "storage-2" {
						continue
					}

					Expect(pg.ExclusionSkipped).To(BeFalse())
				}
			})
		})

		Context("with another in-flight exclusion", func() {
			BeforeEach(func() {
				processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-3")
				processGroup.MarkForRemoval()
			})

			It("should return nil", func() {
				Expect(result).To(BeNil())
			})

			It("should not mark the process group for removal", func() {
				Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-3"}))
			})

			When("max concurrent replacements is set to two", func() {
				BeforeEach(func() {
					cluster.Spec.AutomationOptions.Replacements.MaxConcurrentReplacements = pointer.Int(2)
				})

				It("should requeue", func() {
					Expect(result).NotTo(BeNil())
					Expect(result.message).To(Equal("Removals have been updated in the cluster status"))
				})

				It("should mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-2", "storage-3"}))
				})
			})

			When("max concurrent replacements is set to zero", func() {
				BeforeEach(func() {
					cluster.Spec.AutomationOptions.Replacements.MaxConcurrentReplacements = pointer.Int(0)
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-3"}))
				})
			})
		})

		Context("with another complete exclusion", func() {
			BeforeEach(func() {
				processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-3")
				processGroup.MarkForRemoval()
				processGroup.SetExclude()
			})

			It("should requeue", func() {
				Expect(result).NotTo(BeNil())
				Expect(result.message).To(Equal("Removals have been updated in the cluster status"))
			})

			It("should mark the process group for removal", func() {
				Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-2", "storage-3"}))
			})
		})

		Context("with no addresses", func() {
			BeforeEach(func() {
				processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
				processGroup.Addresses = nil
			})

			It("should requeue", func() {
				Expect(result).NotTo(BeNil())
				Expect(result.message).To(Equal("Removals have been updated in the cluster status"))
			})

			It("should mark the process group for removal", func() {
				Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{"storage-2"}))
			})

			It("should marked to skip exclusion", func() {
				for _, pg := range cluster.Status.ProcessGroups {
					if pg.ProcessGroupID != "storage-2" {
						continue
					}

					Expect(pg.ExclusionSkipped).To(BeTrue())
				}
			})

			When("the cluster is not available", func() {
				BeforeEach(func() {
					processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
					processGroup.Addresses = nil

					adminClient, err := mock.NewMockAdminClientUncast(cluster, k8sClient)
					Expect(err).NotTo(HaveOccurred())
					adminClient.FrozenStatus = &fdbv1beta2.FoundationDBStatus{
						Client: fdbv1beta2.FoundationDBStatusLocalClientInfo{
							DatabaseStatus: fdbv1beta2.FoundationDBStatusClientDBStatus{
								Available: false,
							},
						},
					}
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})

			When("the cluster doesn't have full fault tolerance", func() {
				BeforeEach(func() {
					processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
					processGroup.Addresses = nil

					adminClient, err := mock.NewMockAdminClientUncast(cluster, k8sClient)
					Expect(err).NotTo(HaveOccurred())
					adminClient.MaxZoneFailuresWithoutLosingData = pointer.Int(0)
				})

				It("should return nil", func() {
					Expect(result).To(BeNil())
				})

				It("should not mark the process group for removal", func() {
					Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
				})
			})
		})
	})

	Context("with a process that has been missing for a brief time", func() {
		BeforeEach(func() {
			processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
			processGroup.ProcessGroupConditions = append(processGroup.ProcessGroupConditions, &fdbv1beta2.ProcessGroupCondition{
				ProcessGroupConditionType: fdbv1beta2.MissingProcesses,
				Timestamp:                 time.Now().Unix(),
			})
		})

		It("should return nil", func() {
			Expect(result).To(BeNil())
		})

		It("should not mark the process group for removal", func() {
			Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
		})
	})

	Context("with a process that has had an incorrect pod spec for a long time", func() {
		BeforeEach(func() {
			processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, "storage-2")
			processGroup.ProcessGroupConditions = append(processGroup.ProcessGroupConditions, &fdbv1beta2.ProcessGroupCondition{
				ProcessGroupConditionType: fdbv1beta2.IncorrectPodSpec,
				Timestamp:                 time.Now().Add(-1 * time.Hour).Unix(),
			})
		})

		It("should return nil", func() {
			Expect(result).To(BeNil())
		})

		It("should not mark the process group for removal", func() {
			Expect(getRemovedProcessGroupIDs(cluster)).To(Equal([]fdbv1beta2.ProcessGroupID{}))
		})
	})
})

// getRemovedProcessGroupIDs returns a list of ids for the process groups that
// are marked for removal.
func getRemovedProcessGroupIDs(cluster *fdbv1beta2.FoundationDBCluster) []fdbv1beta2.ProcessGroupID {
	results := make([]fdbv1beta2.ProcessGroupID, 0)
	for _, processGroupStatus := range cluster.Status.ProcessGroups {
		if processGroupStatus.IsMarkedForRemoval() {
			results = append(results, processGroupStatus.ProcessGroupID)
		}
	}
	return results
}
