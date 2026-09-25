package sandbox

import (
	"context"
	"log"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/store"
)

// StatusWarm marks a pooled sandbox: pod running, not yet handed to a user.
// Warm sandboxes are invisible to the API.
const StatusWarm = "warm"

// Only default-shaped sandboxes are pooled. network=none never is: a warm pod
// relabelled on claim could keep internet access until the policy catches up.
func poolable(m store.Meta) bool {
	return m.Image == DefaultImage && m.CPU == "1" && m.Memory == "1Gi" && m.Network == "internet"
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// claim turns a Ready warm sandbox into want, keeping the warm ID. On success
// the sandbox lock is held; the caller must call unlock.
// ponytail: scans every meta.json per claim; keep an in-memory warm list past ~1k sandboxes.
func (m *Manager) claim(ctx context.Context, want store.Meta) (store.Meta, *corev1.Pod, func(), bool) {
	if m.PoolSize <= 0 || !poolable(want) {
		return want, nil, nil, false
	}
	ids, _ := m.Store.IDs()
	for _, id := range ids {
		if meta, err := m.Store.ReadMeta(id); err != nil || meta.Status != StatusWarm {
			continue
		}
		p, err := m.Kube.CoreV1().Pods(Namespace).Get(ctx, id, metav1.GetOptions{})
		if err != nil || !podReady(p) {
			continue
		}
		unlock := m.lock(id)
		if cur, err := m.Store.ReadMeta(id); err != nil || cur.Status != StatusWarm {
			unlock() // claimed by someone else meanwhile
			continue
		}
		want.ID = id
		if err := m.Store.WriteMeta(want); err != nil {
			unlock()
			continue
		}
		m.syncAnnotations(ctx, want)
		go m.FillPool(context.Background())
		return want, p, unlock, true
	}
	return want, nil, nil, false
}

// FillPool starts warm sandboxes until PoolSize exist. Pods come up in the
// background; claim only takes Ready ones.
func (m *Manager) FillPool(ctx context.Context) {
	if m.PoolSize <= 0 || !m.fillMu.TryLock() {
		return
	}
	defer m.fillMu.Unlock()
	if m.checkHeadroom(15) != nil {
		return
	}
	for n := m.warm(); n < m.PoolSize; n++ {
		meta := store.Meta{ID: store.NewID(), Image: DefaultImage, Created: m.Now().UTC(), Status: StatusWarm,
			Network: "internet", CPU: "1", Memory: "1Gi"}
		unlock := m.lock(meta.ID)
		err := m.Store.Create(meta)
		if err == nil {
			_, err = m.ensurePod(ctx, meta, "")
		}
		unlock()
		if err != nil {
			log.Printf("pool: %v", err)
			os.RemoveAll(m.Store.Dir(meta.ID))
			return
		}
	}
}

func (m *Manager) warm() int {
	ids, _ := m.Store.IDs()
	n := 0
	for _, id := range ids {
		if meta, err := m.Store.ReadMeta(id); err == nil && meta.Status == StatusWarm {
			n++
		}
	}
	return n
}

type Status struct {
	FreePct  float64 `json:"free_pct"`
	Warm     int     `json:"warm"`
	PoolSize int     `json:"pool_size"`
}

// Status is server health for the dashboard; warm sandboxes are not in List.
func (m *Manager) Status() (*Status, error) {
	free, err := FreePct(m.Store.Root)
	if err != nil {
		return nil, err
	}
	return &Status{FreePct: free, Warm: m.warm(), PoolSize: m.PoolSize}, nil
}
