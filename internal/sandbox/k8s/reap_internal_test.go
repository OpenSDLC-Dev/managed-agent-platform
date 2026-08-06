package k8s

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// reapRevoker records revokes for the ordering assertions.
type reapRevoker struct {
	revoked []domain.ID
	err     error
	// deletesAtRevoke snapshots how many pod deletes had landed when each
	// revoke arrived — the revoke-before-teardown ordering (#197).
	deletesAtRevoke []int
	deletes         *int
}

func (r *reapRevoker) Revoke(_ context.Context, sid domain.ID) error {
	if r.err != nil {
		return r.err
	}
	r.revoked = append(r.revoked, sid)
	r.deletesAtRevoke = append(r.deletesAtRevoke, *r.deletes)
	return nil
}

// ownedPod is a pod carrying the ownership label, the way podSpec stamps one.
func ownedPod(name string, sid domain.ID) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "default",
		Labels:    map[string]string{sessionLabel: string(sid)},
		UID:       types.UID("uid-" + name),
	}}
}

// TestReapRevokesThenDeletesPod: the session's token is revoked before its pod
// is deleted, the delete lands, and the reap reports success only once the pod
// is actually gone (deleteAndWaitGone).
func TestReapRevokesThenDeletesPod(t *testing.T) {
	sid := domain.NewID("sesn")
	deletes := 0
	rev := &reapRevoker{deletes: &deletes}
	p := fakeProvider(ownedPod(podName(sid), sid))
	p.revoker = rev
	p.client.cs.(*fake.Clientset).PrependReactor("delete", "pods",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			deletes++
			return false, nil, nil // fall through to the tracker's real delete
		})

	if err := p.Reap(context.Background(), sid); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(rev.revoked, []domain.ID{sid}) {
		t.Errorf("revoked = %v, want [%s]", rev.revoked, sid)
	}
	if !slices.Equal(rev.deletesAtRevoke, []int{0}) {
		t.Errorf("revoke landed after %v deletes, want before any", rev.deletesAtRevoke)
	}
	if deletes != 1 {
		t.Errorf("deletes = %d, want 1", deletes)
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); err == nil {
		t.Error("pod still present after reap")
	}
}

// TestReapRevokeFailureStopsBeforeDeleting: a failed revoke aborts with the pod
// intact, so the next pass retries both halves.
func TestReapRevokeFailureStopsBeforeDeleting(t *testing.T) {
	sid := domain.NewID("sesn")
	deletes := 0
	p := fakeProvider(ownedPod(podName(sid), sid))
	p.revoker = &reapRevoker{err: errors.New("db down"), deletes: &deletes}
	if err := p.Reap(context.Background(), sid); err == nil {
		t.Fatal("reap succeeded despite a failed revoke")
	}
	if _, err := p.client.cs.CoreV1().Pods("default").Get(context.Background(), podName(sid), metav1.GetOptions{}); err != nil {
		t.Errorf("pod deleted despite a failed revoke: %v", err)
	}
}

// TestReapNothingOwnedStillRevokes: no pod is a delete no-op, but the token is
// platform state and may outlive the pod — the revoke still lands.
func TestReapNothingOwnedStillRevokes(t *testing.T) {
	sid := domain.NewID("sesn")
	deletes := 0
	rev := &reapRevoker{deletes: &deletes}
	p := fakeProvider()
	p.revoker = rev
	if err := p.Reap(context.Background(), sid); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(rev.revoked, []domain.ID{sid}) {
		t.Errorf("revoked = %v, want [%s]", rev.revoked, sid)
	}
}

// TestReapSelectsByLabelNotName: the pod is found by its ownership label — a
// pod whose name does not follow podName's derivation (a future renaming, a
// manual recreation) is still the session's to reap, and another session's pod
// is untouched.
func TestReapSelectsByLabelNotName(t *testing.T) {
	sid, other := domain.NewID("sesn"), domain.NewID("sesn")
	p := fakeProvider(ownedPod("oddly-named", sid), ownedPod(podName(other), other))
	if err := p.Reap(context.Background(), sid); err != nil {
		t.Fatalf("reap: %v", err)
	}
	pods := p.client.cs.CoreV1().Pods("default")
	if _, err := pods.Get(context.Background(), "oddly-named", metav1.GetOptions{}); err == nil {
		t.Error("the session's oddly-named pod survived the reap")
	}
	if _, err := pods.Get(context.Background(), podName(other), metav1.GetOptions{}); err != nil {
		t.Errorf("another session's pod was reaped: %v", err)
	}
}

// TestReapSurfacesDeleteFailure: a pod the API refuses to delete surfaces as
// Reap's error — a reap that swallows it would report a still-running sandbox
// as reaped, and its caller would stop retrying.
func TestReapSurfacesDeleteFailure(t *testing.T) {
	sid := domain.NewID("sesn")
	p := fakeProvider(ownedPod(podName(sid), sid))
	stuck := errors.New("node partitioned")
	p.client.cs.(*fake.Clientset).PrependReactor("delete", "pods",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stuck
		})
	if err := p.Reap(context.Background(), sid); !errors.Is(err, stuck) {
		t.Errorf("reap error = %v, want the delete failure surfaced", err)
	}
}

// TestOwnedListsDistinctSessionsFromLabels: Owned reads the label, dedups, and
// ignores pods without it — including a pod whose label key is present with an
// empty value, which the presence selector still returns.
func TestOwnedListsDistinctSessionsFromLabels(t *testing.T) {
	sidA, sidB := domain.NewID("sesn"), domain.NewID("sesn")
	unlabeled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: "default"}}
	emptyValue := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "empty-labeled", Namespace: "default",
		Labels: map[string]string{sessionLabel: ""},
	}}
	p := fakeProvider(ownedPod(podName(sidA), sidA), ownedPod("second-holding", sidA),
		ownedPod(podName(sidB), sidB), unlabeled, emptyValue)
	owned, err := p.Owned(context.Background())
	if err != nil {
		t.Fatalf("owned: %v", err)
	}
	slices.Sort(owned)
	want := []domain.ID{sidA, sidB}
	slices.Sort(want)
	if !slices.Equal(owned, want) {
		t.Errorf("owned = %v, want %v", owned, want)
	}
}
