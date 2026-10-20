package kwok

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
)

// TestTransitionAfterTheControllerMarkedUnknown: a host is powered off and stays off long enough
// for the node-lifecycle controller to mark Ready Unknown (the twin renews no lease for it); when
// it powers on, Ready goes Unknown -> False (PoweringOn), and that is a transition now, as a
// kubelet computes it against the Node it reads back. compat/medik8s found the projection keeping
// the time Ready first became False, so NHC read the powering-on host as False for minutes.
func TestTransitionAfterTheControllerMarkedUnknown(t *testing.T) {
	ctx := context.Background()
	e := labEngine(t)
	srv := device(t, e, "b02/server-02")
	a := New()
	a.rt = &adapter.Runtime{Engine: e, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cs := fake.NewSimpleClientset()
	a.cs = cs
	h := &host{d: srv, name: NodeName(srv), static: static(e, srv),
		condSince: map[corev1.NodeConditionType]metav1.Time{}, condWas: map[corev1.NodeConditionType]corev1.ConditionStatus{}}
	ready := func() corev1.NodeCondition {
		n, err := cs.CoreV1().Nodes().Get(ctx, h.name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c
			}
		}
		t.Fatal("no Ready condition")
		return corev1.NodeCondition{}
	}
	if err := a.sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	if c := ready(); c.Status != corev1.ConditionTrue {
		t.Fatalf("running: %+v", c)
	}
	if err := e.Power(engine.Request{Path: srv.Path, Canonical: "power.off.force", Source: src}); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	if c := ready(); c.Status != corev1.ConditionFalse || c.Reason != ReasonPoweredOff {
		t.Fatalf("powered off: %+v", c)
	}
	// The outage began an hour ago, and the controller marked the Node Unknown 50 s later.
	offAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	h.condSince[corev1.NodeReady] = offAt
	n, err := cs.CoreV1().Nodes().Get(ctx, h.name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			n.Status.Conditions[i] = corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown,
				Reason: "NodeStatusUnknown", Message: "Kubelet stopped posting node status.",
				LastHeartbeatTime: c.LastHeartbeatTime, LastTransitionTime: metav1.NewTime(offAt.Add(50 * time.Second))}
		}
	}
	if _, err := cs.CoreV1().Nodes().UpdateStatus(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Power on (no clock runs here, so the host stays PoweringOn): Ready is False again.
	if err := e.Power(engine.Request{Path: srv.Path, Canonical: "power.on", Source: src}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	if err := a.sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	c := ready()
	if c.Status != corev1.ConditionFalse || c.Reason != ReasonPoweringOn {
		t.Fatalf("powering on: %+v", c)
	}
	if c.LastTransitionTime.Time.Before(before) {
		t.Fatalf("Unknown -> False at power-on kept transition time %v (the outage began %v); want now", c.LastTransitionTime, offAt)
	}
}
