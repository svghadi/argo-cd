package e2e

import (
	"testing"

	. "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/argoproj/argo-cd/v2/test/e2e/fixture"
	. "github.com/argoproj/argo-cd/v2/test/e2e/fixture/app"
	v1 "k8s.io/api/core/v1"

	"github.com/argoproj/gitops-engine/pkg/health"
	. "github.com/argoproj/gitops-engine/pkg/sync/common"
)

func TestFixingDegradedApp(t *testing.T) {
	Given(t).
		Path("sync-waves").
		When().
		IgnoreErrors().
		CreateApp().
		And(func() {
			SetResourceOverrides(map[string]ResourceOverride{
				"ConfigMap": {
					HealthLua: `return { status = obj.metadata.annotations and obj.metadata.annotations['health'] or 'Degraded' }`,
				},
			})
		}).
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationFailed)).
		Expect(SyncStatusIs(SyncStatusCodeOutOfSync)).
		Expect(HealthIs(health.HealthStatusDegraded)).
		Expect(ResourceResultNumbering(1)).
		Expect(ResourceSyncStatusIs("ConfigMap", "cm-1", SyncStatusCodeSynced)).
		Expect(ResourceHealthIs("ConfigMap", "cm-1", health.HealthStatusDegraded)).
		Expect(ResourceSyncStatusIs("ConfigMap", "cm-2", SyncStatusCodeOutOfSync)).
		Expect(ResourceHealthIs("ConfigMap", "cm-2", health.HealthStatusMissing)).
		When().
		PatchFile("cm-1.yaml", `[{"op": "replace", "path": "/metadata/annotations/health", "value": "Healthy"}]`).
		PatchFile("cm-2.yaml", `[{"op": "replace", "path": "/metadata/annotations/health", "value": "Healthy"}]`).
		// need to force a refresh here
		Refresh(RefreshTypeNormal).
		Then().
		Expect(ResourceSyncStatusIs("ConfigMap", "cm-1", SyncStatusCodeOutOfSync)).
		When().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(HealthIs(health.HealthStatusHealthy)).
		Expect(ResourceResultNumbering(2)).
		Expect(ResourceSyncStatusIs("ConfigMap", "cm-1", SyncStatusCodeSynced)).
		Expect(ResourceHealthIs("ConfigMap", "cm-1", health.HealthStatusHealthy)).
		Expect(ResourceSyncStatusIs("ConfigMap", "cm-2", SyncStatusCodeSynced)).
		Expect(ResourceHealthIs("ConfigMap", "cm-2", health.HealthStatusHealthy))
}

func TestOneProgressingDeploymentIsSucceededAndSynced(t *testing.T) {
	Given(t).
		Path("one-deployment").
		When().
		// make this deployment get stuck in progressing due to "invalidimagename"
		PatchFile("deployment.yaml", `[
    {
        "op": "replace",
        "path": "/spec/template/spec/containers/0/image",
        "value": "alpine:ops!"
    }
]`).
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(HealthIs(health.HealthStatusProgressing)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(ResourceResultNumbering(1))
}

func TestDegradedDeploymentIsSucceededAndSynced(t *testing.T) {
	Given(t).
		Path("one-deployment").
		When().
		// make this deployment get stuck in progressing due to "invalidimagename"
		PatchFile("deployment.yaml", `[
    {
        "op": "replace",
        "path": "/spec/progressDeadlineSeconds",
        "value": 1
    },
    {
        "op": "replace",
        "path": "/spec/template/spec/containers/0/image",
        "value": "alpine:ops!"
    }
]`).
		CreateApp().
		Sync().
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(HealthIs(health.HealthStatusDegraded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(ResourceResultNumbering(1))
}

func TestSyncPruneOrderWithSyncWaves(t *testing.T) {
	ctx := Given(t)

	// ensure proper cleanup if test fails at early stage
	defer RunCli("app", "patch-resource", ctx.AppQualifiedName(), 
		"--kind", "Pod", 
		"--resource-name", "pod-2", 
		"--patch", `[{"op": "remove", "path": "/metadata/finalizers"}]`,
		"--patch-type", "application/json-patch+json", "--all",
	)

	ctx.Path("two-nice-pods").
		When().
		PatchFile("pod-1.yaml", `[{"op": "add", "path": "/metadata/annotations", "value": {"argocd.argoproj.io/sync-wave": "1"}}]`).
		PatchFile("pod-2.yaml", `[{"op": "add", "path": "/metadata/annotations", "value": {"argocd.argoproj.io/sync-wave": "2"}}]`).
		CreateApp().
		Sync().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(HealthIs(health.HealthStatusHealthy)).
		When().
		// delete files to remove pods
		DeleteFile("pod-1.yaml").
		DeleteFile("pod-2.yaml").
		// add a finalizer on live pod2 to block cleanup
		PatchAppResource("Pod", "pod-2", `[{"op": "add", "path": "/metadata/finalizers", "value": ["example.com/block-deletion"]}]`, "--patch-type", "application/json-patch+json").
		AddFile("dummy.txt", `# add a dummy file to prevent failure due to "two-nice-pods: app path does not exist" error`).
		Refresh(RefreshTypeHard).
		IgnoreErrors().
		Then().
		Expect(SyncStatusIs(SyncStatusCodeOutOfSync)).
		When().
		// prune order: pod2 -> pod1
		Sync("--prune", "--async").
		Then().
		Expect(OperationPhaseIs(OperationRunning)).
		Expect(ResourceResultCodeIs("Pod", "pod-2", ResultCodePruned)).
		Expect(ResourceResultCodeIs("Pod", "pod-1", "")).
		Expect(Pod(func(p v1.Pod) bool { return p.Name == "pod-2" && p.GetDeletionTimestamp() != nil })).
		Expect(Pod(func(p v1.Pod) bool { return p.Name == "pod-1" && p.GetDeletionTimestamp() == nil })).
		When().
		// remove finalizer on pod-2 to delete the pod from cluster
		PatchAppResource("Pod", "pod-2", `[{"op": "remove", "path": "/metadata/finalizers"}]`, "--patch-type", "application/json-patch+json").
		Wait().
		Refresh(RefreshTypeHard).
		Then().
		Expect(OperationPhaseIs(OperationSucceeded)).
		Expect(SyncStatusIs(SyncStatusCodeSynced)).
		Expect(ResourceResultCodeIs("Pod", "pod-2", ResultCodePruned)).
		Expect(ResourceResultCodeIs("Pod", "pod-1", ResultCodePruned)).
		Expect(NotPod(func(p v1.Pod) bool { return p.Name == "pod-2" })).
		Expect(NotPod(func(p v1.Pod) bool { return p.Name == "pod-1" })).
		Expect(HealthIs(health.HealthStatusHealthy))

}
