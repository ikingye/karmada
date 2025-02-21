package helper

import (
	"context"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1alpha1 "github.com/karmada-io/karmada/pkg/apis/cluster/v1alpha1"
	"github.com/karmada-io/karmada/pkg/apis/policy/v1alpha1"
	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	"github.com/karmada-io/karmada/pkg/util"
	"github.com/karmada-io/karmada/pkg/util/names"
	"github.com/karmada-io/karmada/pkg/util/overridemanager"
	"github.com/karmada-io/karmada/pkg/util/restmapper"
)

const (
	// SpecField indicates the 'spec' field of a deployment
	SpecField = "spec"
	// ReplicasField indicates the 'replicas' field of a deployment
	ReplicasField = "replicas"
)

// ClusterWeightInfo records the weight of a cluster
type ClusterWeightInfo struct {
	ClusterName string
	Weight      int64
}

// ClusterWeightInfoList is a slice of ClusterWeightInfo that implements sort.Interface to sort by Value.
type ClusterWeightInfoList []ClusterWeightInfo

func (p ClusterWeightInfoList) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p ClusterWeightInfoList) Len() int      { return len(p) }
func (p ClusterWeightInfoList) Less(i, j int) bool {
	if p[i].Weight != p[j].Weight {
		return p[i].Weight > p[j].Weight
	}
	return p[i].ClusterName < p[j].ClusterName
}

// SortClusterByWeight sort clusters by the weight
func SortClusterByWeight(m map[string]int64) ClusterWeightInfoList {
	p := make(ClusterWeightInfoList, len(m))
	i := 0
	for k, v := range m {
		p[i] = ClusterWeightInfo{k, v}
		i++
	}
	sort.Sort(p)
	return p
}

// IsBindingReady will check if resourceBinding/clusterResourceBinding is ready to build Work.
func IsBindingReady(targetClusters []workv1alpha1.TargetCluster) bool {
	return len(targetClusters) != 0
}

// HasScheduledReplica checks if the scheduler has assigned replicas for each cluster.
func HasScheduledReplica(scheduleResult []workv1alpha1.TargetCluster) bool {
	for _, clusterResult := range scheduleResult {
		if clusterResult.Replicas > 0 {
			return true
		}
	}
	return false
}

// GetBindingClusterNames will get clusterName list from bind clusters field
func GetBindingClusterNames(targetClusters []workv1alpha1.TargetCluster) []string {
	var clusterNames []string
	for _, targetCluster := range targetClusters {
		clusterNames = append(clusterNames, targetCluster.Name)
	}
	return clusterNames
}

// FindOrphanWorks retrieves all works that labeled with current binding(ResourceBinding or ClusterResourceBinding) objects,
// then pick the works that not meet current binding declaration.
func FindOrphanWorks(c client.Client, bindingNamespace, bindingName string, clusterNames []string, scope apiextensionsv1.ResourceScope) ([]workv1alpha1.Work, error) {
	workList := &workv1alpha1.WorkList{}
	if scope == apiextensionsv1.NamespaceScoped {
		selector := labels.SelectorFromSet(labels.Set{
			util.ResourceBindingNamespaceLabel: bindingNamespace,
			util.ResourceBindingNameLabel:      bindingName,
		})

		if err := c.List(context.TODO(), workList, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, err
		}
	} else {
		selector := labels.SelectorFromSet(labels.Set{
			util.ClusterResourceBindingLabel: bindingName,
		})

		if err := c.List(context.TODO(), workList, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, err
		}
	}

	var orphanWorks []workv1alpha1.Work
	expectClusters := sets.NewString(clusterNames...)
	for _, work := range workList.Items {
		workTargetCluster, err := names.GetClusterName(work.GetNamespace())
		if err != nil {
			klog.Errorf("Failed to get cluster name which Work %s/%s belongs to. Error: %v.",
				work.GetNamespace(), work.GetName(), err)
			return nil, err
		}
		if !expectClusters.Has(workTargetCluster) {
			orphanWorks = append(orphanWorks, work)
		}
	}
	return orphanWorks, nil
}

// RemoveOrphanWorks will remove orphan works.
func RemoveOrphanWorks(c client.Client, works []workv1alpha1.Work) error {
	for workIndex, work := range works {
		err := c.Delete(context.TODO(), &works[workIndex])
		if err != nil {
			return err
		}
		klog.Infof("Delete orphan work %s/%s successfully.", work.GetNamespace(), work.GetName())
	}
	return nil
}

// FetchWorkload fetches the kubernetes resource to be propagated.
func FetchWorkload(dynamicClient dynamic.Interface, restMapper meta.RESTMapper, resource workv1alpha1.ObjectReference) (*unstructured.Unstructured, error) {
	dynamicResource, err := restmapper.GetGroupVersionResource(restMapper,
		schema.FromAPIVersionAndKind(resource.APIVersion, resource.Kind))
	if err != nil {
		klog.Errorf("Failed to get GVR from GVK %s %s. Error: %v", resource.APIVersion,
			resource.Kind, err)
		return nil, err
	}

	workload, err := dynamicClient.Resource(dynamicResource).Namespace(resource.Namespace).Get(context.TODO(),
		resource.Name, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get workload, kind: %s, namespace: %s, name: %s. Error: %v",
			resource.Kind, resource.Namespace, resource.Name, err)
		return nil, err
	}

	return workload, nil
}

// EnsureWork ensure Work to be created or updated.
func EnsureWork(c client.Client, workload *unstructured.Unstructured, overrideManager overridemanager.OverrideManager, binding metav1.Object, scope apiextensionsv1.ResourceScope) error {
	var targetClusters []workv1alpha1.TargetCluster
	switch scope {
	case apiextensionsv1.NamespaceScoped:
		bindingObj := binding.(*workv1alpha1.ResourceBinding)
		targetClusters = bindingObj.Spec.Clusters
	case apiextensionsv1.ClusterScoped:
		bindingObj := binding.(*workv1alpha1.ClusterResourceBinding)
		targetClusters = bindingObj.Spec.Clusters
	}

	hasScheduledReplica, referenceRSP, desireReplicaInfos, err := getRSPAndReplicaInfos(c, workload, targetClusters)
	if err != nil {
		return err
	}

	for _, targetCluster := range targetClusters {
		clonedWorkload := workload.DeepCopy()
		cops, ops, err := overrideManager.ApplyOverridePolicies(clonedWorkload, targetCluster.Name)
		if err != nil {
			klog.Errorf("Failed to apply overrides for %s/%s/%s, err is: %v", clonedWorkload.GetKind(), clonedWorkload.GetNamespace(), clonedWorkload.GetName(), err)
			return err
		}

		workNamespace, err := names.GenerateExecutionSpaceName(targetCluster.Name)
		if err != nil {
			klog.Errorf("Failed to ensure Work for cluster: %s. Error: %v.", targetCluster.Name, err)
			return err
		}

		workLabel := mergeLabel(clonedWorkload, workNamespace, binding, scope)

		if clonedWorkload.GetKind() == util.DeploymentKind && (referenceRSP != nil || hasScheduledReplica) {
			err = applyReplicaSchedulingPolicy(clonedWorkload, desireReplicaInfos[targetCluster.Name])
			if err != nil {
				klog.Errorf("failed to apply ReplicaSchedulingPolicy for %s/%s/%s in cluster %s, err is: %v",
					clonedWorkload.GetKind(), clonedWorkload.GetNamespace(), clonedWorkload.GetName(), targetCluster.Name, err)
				return err
			}
		}

		annotations, err := recordAppliedOverrides(cops, ops)
		if err != nil {
			klog.Errorf("failed to record appliedOverrides, Error: %v", err)
			return err
		}

		workMeta := metav1.ObjectMeta{
			Name:        names.GenerateWorkName(clonedWorkload.GetKind(), clonedWorkload.GetName(), clonedWorkload.GetNamespace()),
			Namespace:   workNamespace,
			Finalizers:  []string{util.ExecutionControllerFinalizer},
			Labels:      workLabel,
			Annotations: annotations,
		}

		if err = CreateOrUpdateWork(c, workMeta, clonedWorkload); err != nil {
			return err
		}
	}
	return nil
}

func getRSPAndReplicaInfos(c client.Client, workload *unstructured.Unstructured, targetClusters []workv1alpha1.TargetCluster) (bool, *v1alpha1.ReplicaSchedulingPolicy, map[string]int64, error) {
	if HasScheduledReplica(targetClusters) {
		return true, nil, transScheduleResultToMap(targetClusters), nil
	}

	referenceRSP, desireReplicaInfos, err := calculateReplicasIfNeeded(c, workload, GetBindingClusterNames(targetClusters))
	if err != nil {
		klog.Errorf("Failed to get ReplicaSchedulingPolicy for %s/%s/%s, err is: %v", workload.GetKind(), workload.GetNamespace(), workload.GetName(), err)
		return false, nil, nil, err
	}

	return false, referenceRSP, desireReplicaInfos, nil
}

func mergeLabel(workload *unstructured.Unstructured, workNamespace string, binding metav1.Object, scope apiextensionsv1.ResourceScope) map[string]string {
	var workLabel = make(map[string]string)
	util.MergeLabel(workload, util.WorkNamespaceLabel, workNamespace)
	util.MergeLabel(workload, util.WorkNameLabel, names.GenerateWorkName(workload.GetKind(), workload.GetName(), workload.GetNamespace()))

	if scope == apiextensionsv1.NamespaceScoped {
		util.MergeLabel(workload, util.ResourceBindingNamespaceLabel, binding.GetNamespace())
		util.MergeLabel(workload, util.ResourceBindingNameLabel, binding.GetName())
		workLabel[util.ResourceBindingNamespaceLabel] = binding.GetNamespace()
		workLabel[util.ResourceBindingNameLabel] = binding.GetName()
	} else {
		util.MergeLabel(workload, util.ClusterResourceBindingLabel, binding.GetName())
		workLabel[util.ClusterResourceBindingLabel] = binding.GetName()
	}

	return workLabel
}

func recordAppliedOverrides(cops *overridemanager.AppliedOverrides, ops *overridemanager.AppliedOverrides) (map[string]string, error) {
	annotations := make(map[string]string)

	if cops != nil {
		appliedBytes, err := cops.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if appliedBytes != nil {
			annotations[util.AppliedClusterOverrides] = string(appliedBytes)
		}
	}

	if ops != nil {
		appliedBytes, err := ops.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if appliedBytes != nil {
			annotations[util.AppliedOverrides] = string(appliedBytes)
		}
	}

	return annotations, nil
}

func transScheduleResultToMap(scheduleResult []workv1alpha1.TargetCluster) map[string]int64 {
	var desireReplicaInfos = make(map[string]int64, len(scheduleResult))
	for _, clusterInfo := range scheduleResult {
		desireReplicaInfos[clusterInfo.Name] = int64(clusterInfo.Replicas)
	}
	return desireReplicaInfos
}

func calculateReplicasIfNeeded(c client.Client, workload *unstructured.Unstructured, clusterNames []string) (*v1alpha1.ReplicaSchedulingPolicy, map[string]int64, error) {
	var err error
	var referenceRSP *v1alpha1.ReplicaSchedulingPolicy
	var desireReplicaInfos = make(map[string]int64)

	if workload.GetKind() == util.DeploymentKind {
		referenceRSP, err = matchReplicaSchedulingPolicy(c, workload)
		if err != nil {
			return nil, nil, err
		}
		if referenceRSP != nil {
			desireReplicaInfos, err = calculateReplicas(c, referenceRSP, clusterNames)
			if err != nil {
				klog.Errorf("Failed to get desire replicas for %s/%s/%s, err is: %v", workload.GetKind(), workload.GetNamespace(), workload.GetName(), err)
				return nil, nil, err
			}
			klog.V(4).Infof("DesireReplicaInfos with replica scheduling policies(%s/%s) is %v", referenceRSP.Namespace, referenceRSP.Name, desireReplicaInfos)
		}
	}
	return referenceRSP, desireReplicaInfos, nil
}

func matchReplicaSchedulingPolicy(c client.Client, workload *unstructured.Unstructured) (*v1alpha1.ReplicaSchedulingPolicy, error) {
	// get all namespace-scoped replica scheduling policies
	policyList := &v1alpha1.ReplicaSchedulingPolicyList{}
	if err := c.List(context.TODO(), policyList, &client.ListOptions{Namespace: workload.GetNamespace()}); err != nil {
		klog.Errorf("Failed to list replica scheduling policies from namespace: %s, error: %v", workload.GetNamespace(), err)
		return nil, err
	}

	if len(policyList.Items) == 0 {
		return nil, nil
	}

	matchedPolicies := getMatchedReplicaSchedulingPolicy(policyList.Items, workload)
	if len(matchedPolicies) == 0 {
		klog.V(2).Infof("No replica scheduling policy for resource: %s/%s", workload.GetNamespace(), workload.GetName())
		return nil, nil
	}

	return &matchedPolicies[0], nil
}

func getMatchedReplicaSchedulingPolicy(policies []v1alpha1.ReplicaSchedulingPolicy, resource *unstructured.Unstructured) []v1alpha1.ReplicaSchedulingPolicy {
	// select policy in which at least one resource selector matches target resource.
	resourceMatches := make([]v1alpha1.ReplicaSchedulingPolicy, 0)
	for _, policy := range policies {
		if util.ResourceMatchSelectors(resource, policy.Spec.ResourceSelectors...) {
			resourceMatches = append(resourceMatches, policy)
		}
	}

	// Sort by policy names.
	sort.Slice(resourceMatches, func(i, j int) bool {
		return resourceMatches[i].Name < resourceMatches[j].Name
	})

	return resourceMatches
}

func calculateReplicas(c client.Client, policy *v1alpha1.ReplicaSchedulingPolicy, clusterNames []string) (map[string]int64, error) {
	weightSum := int64(0)
	matchClusters := make(map[string]int64)
	desireReplicaInfos := make(map[string]int64)

	// found out clusters matched the given ReplicaSchedulingPolicy
	for _, clusterName := range clusterNames {
		clusterObj := &clusterv1alpha1.Cluster{}
		if err := c.Get(context.TODO(), client.ObjectKey{Name: clusterName}, clusterObj); err != nil {
			klog.Errorf("Failed to get member cluster: %s, error: %v", clusterName, err)
			return nil, err
		}
		for _, staticWeightRule := range policy.Spec.Preferences.StaticWeightList {
			if util.ClusterMatches(clusterObj, staticWeightRule.TargetCluster) {
				weightSum += staticWeightRule.Weight
				matchClusters[clusterName] = staticWeightRule.Weight
				break
			}
		}
	}

	allocatedReplicas := int32(0)
	for clusterName, weight := range matchClusters {
		desireReplicaInfos[clusterName] = weight * int64(policy.Spec.TotalReplicas) / weightSum
		allocatedReplicas += int32(desireReplicaInfos[clusterName])
	}

	if remainReplicas := policy.Spec.TotalReplicas - allocatedReplicas; remainReplicas > 0 {
		sortedClusters := SortClusterByWeight(matchClusters)
		for i := 0; remainReplicas > 0; i++ {
			desireReplicaInfos[sortedClusters[i].ClusterName]++
			remainReplicas--
			if i == len(desireReplicaInfos) {
				i = 0
			}
		}
	}

	for _, clusterName := range clusterNames {
		if _, exist := matchClusters[clusterName]; !exist {
			desireReplicaInfos[clusterName] = 0
		}
	}

	return desireReplicaInfos, nil
}

func applyReplicaSchedulingPolicy(workload *unstructured.Unstructured, desireReplica int64) error {
	_, ok, err := unstructured.NestedInt64(workload.Object, SpecField, ReplicasField)
	if err != nil {
		return err
	}
	if ok {
		err := unstructured.SetNestedField(workload.Object, desireReplica, SpecField, ReplicasField)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetClusterResourceBindings returns a ClusterResourceBindingList by labels.
func GetClusterResourceBindings(c client.Client, ls labels.Set) (*workv1alpha1.ClusterResourceBindingList, error) {
	bindings := &workv1alpha1.ClusterResourceBindingList{}
	listOpt := &client.ListOptions{LabelSelector: labels.SelectorFromSet(ls)}

	return bindings, c.List(context.TODO(), bindings, listOpt)
}

// GetResourceBindings returns a ResourceBindingList by labels
func GetResourceBindings(c client.Client, ls labels.Set) (*workv1alpha1.ResourceBindingList, error) {
	bindings := &workv1alpha1.ResourceBindingList{}
	listOpt := &client.ListOptions{LabelSelector: labels.SelectorFromSet(ls)}

	return bindings, c.List(context.TODO(), bindings, listOpt)
}

// GetWorks returns a WorkList by labels
func GetWorks(c client.Client, ls labels.Set) (*workv1alpha1.WorkList, error) {
	works := &workv1alpha1.WorkList{}
	listOpt := &client.ListOptions{LabelSelector: labels.SelectorFromSet(ls)}

	return works, c.List(context.TODO(), works, listOpt)
}

// DeleteWorks will delete all Work objects by labels.
func DeleteWorks(c client.Client, selector labels.Set) (controllerruntime.Result, error) {
	workList, err := GetWorks(c, selector)
	if err != nil {
		klog.Errorf("Failed to get works by label %v: %v", selector, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	var errs []error
	for index, work := range workList.Items {
		if err := c.Delete(context.TODO(), &workList.Items[index]); err != nil {
			klog.Errorf("Failed to delete work(%s/%s): %v", work.Namespace, work.Name, err)
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return controllerruntime.Result{Requeue: true}, errors.NewAggregate(errs)
	}

	return controllerruntime.Result{}, nil
}
