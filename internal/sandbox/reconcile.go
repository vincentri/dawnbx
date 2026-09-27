package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/store"
)

// FreePct reports free space on the data volume. Replaced in tests.
var FreePct = func(root string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, err
	}
	return 100 * float64(st.Bavail) / float64(st.Blocks), nil
}

// DiskUsage sums allocated bytes under dir without following symlinks. Replaced in tests.
var DiskUsage = func(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			} else {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func (m *Manager) checkHeadroom(min float64) error {
	free, err := FreePct(m.Store.Root)
	if err != nil {
		return err
	}
	if free < min {
		return errf(507, "disk_low", "kill unused sandboxes (dawnbx ls) or grow the data volume",
			"data volume %.0f%% free; new sandboxes need %.0f%%", free, min)
	}
	return nil
}

// checkNode is checkHeadroom(AdmitFreePct) for the disk meta's workspace lives on. A
// worker's free space comes from the last reconcile tick.
func (m *Manager) checkNode(meta store.Meta) error {
	if m.local(meta) {
		return m.checkHeadroom(AdmitFreePct)
	}
	if slices.Contains(m.lowDisk(), meta.Node) {
		return errf(507, "disk_low", "kill unused sandboxes on that node (dawnbx ls) or grow its disk",
			"node %s is under 15%% free", meta.Node)
	}
	return nil
}

// Run reconciles once at startup, then every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.Reconcile(ctx, true)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Reconcile(ctx, false)
		}
	}
}

// Reconcile brings pods in line with meta.json. Order per tick:
// TTL -> per-sandbox disk cap -> volume headroom -> pod/label drift.
func (m *Manager) Reconcile(ctx context.Context, startup bool) {
	ids, err := m.Store.IDs()
	if err != nil {
		log.Printf("reconcile: list: %v", err)
		return
	}
	now := m.Now()
	var metas []store.Meta
	for _, id := range ids {
		meta, err := m.Store.ReadMeta(id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Create writes meta right after mkdir; only a crash leaves an old dir without it.
			if info, err := os.Stat(m.Store.Dir(id)); err == nil && now.Sub(info.ModTime()) > 10*time.Minute {
				log.Printf("reconcile: %s: no meta.json, removing orphan dir", id)
				os.RemoveAll(m.Store.Dir(id))
			}
			continue
		case errors.Is(err, store.ErrUnknownVersion):
			log.Printf("reconcile: %s: written by a newer dawnbx, leaving untouched", id)
			continue
		case err != nil:
			log.Printf("reconcile: %s: %v", id, err)
			continue
		}
		metas = append(metas, meta)
	}

	// 1. TTL, and finishing kills interrupted by a crash.
	live := metas[:0]
	for _, meta := range metas {
		if meta.Status == StatusDeleting || (meta.ExpiresAt != nil && now.After(*meta.ExpiresAt)) {
			m.withLock(meta.ID, func(cur store.Meta) error {
				if cur.Status == StatusDeleting || (cur.ExpiresAt != nil && m.Now().After(*cur.ExpiresAt)) {
					return m.remove(ctx, cur)
				}
				return nil // extended since the scan
			})
			continue
		}
		live = append(live, meta)
	}
	metas = live

	// 2. Per-sandbox disk cap (R20).
	usage := map[string]int64{}
	for i, meta := range metas {
		if m.local(meta) {
			usage[meta.ID] = DiskUsage(m.Store.WS(meta.ID))
		} else if meta.Status == StatusRunning || meta.Status == StatusWarm {
			usage[meta.ID] = m.remoteUsage(ctx, meta.ID)
		}
		inGrace := meta.GraceUntil != nil && now.Before(*meta.GraceUntil)
		if meta.Status != StatusStopped && usage[meta.ID] > DiskLimit && !inGrace {
			log.Printf("reconcile: %s uses %d bytes, over cap; stopping", meta.ID, usage[meta.ID])
			m.withLock(meta.ID, func(cur store.Meta) error {
				if cur.Status == StatusStopped || (cur.GraceUntil != nil && m.Now().Before(*cur.GraceUntil)) {
					return nil
				}
				return m.stop(ctx, cur, "over_disk_limit")
			})
			metas[i].Status = StatusStopped
		}
	}

	disks := m.workerDisks(ctx)

	// 3. Volume headroom per node (R11/R17): below 10% free, delete expiring
	// sandboxes largest first, then stop the largest keep-forever one. Only
	// sandboxes on that node's disk free it. A worker's free space is measured
	// once per tick, so what deletes free there is estimated from their usage.
	free := map[string]func() float64{m.Self: func() float64 {
		f, err := FreePct(m.Store.Root)
		if err != nil {
			return 100
		}
		return f
	}}
	for n, d := range disks {
		free[n] = d.freePct
	}
	nodeOf := func(meta store.Meta) string {
		if m.local(meta) {
			return m.Self
		}
		return meta.Node
	}
	sort.Slice(metas, func(i, j int) bool { return usage[metas[i].ID] > usage[metas[j].ID] })
	for node, freeNow := range free {
		if freeNow() >= ReclaimFreePct {
			continue
		}
		for i, meta := range metas {
			if nodeOf(meta) != node || meta.ExpiresAt == nil || meta.Status == StatusDeleting {
				continue
			}
			f := freeNow()
			if f >= ReclaimFreePct {
				break
			}
			log.Printf("reconcile: volume %.1f%% free on %q, deleting %s", f, node, meta.ID)
			m.withLock(meta.ID, func(cur store.Meta) error { return m.remove(ctx, cur) })
			metas[i].Status = StatusDeleting
			if d := disks[node]; d != nil {
				d.freed += usage[meta.ID]
			}
		}
		if f := freeNow(); f < ReclaimFreePct {
			for i, meta := range metas {
				if nodeOf(meta) == node && meta.ExpiresAt == nil && meta.Status == StatusRunning {
					log.Printf("reconcile: volume %.1f%% free on %q, stopping %s", f, node, meta.ID)
					m.withLock(meta.ID, func(cur store.Meta) error { return m.stop(ctx, cur, "disk_full") })
					metas[i].Status = StatusStopped
					break
				}
			}
		}
	}

	var low []string
	for n, d := range disks {
		if d.freePct() < AdmitFreePct {
			low = append(low, n)
		}
	}
	m.mu.Lock()
	m.low = low
	m.mu.Unlock()
	// Warm sandboxes on a low worker can't be claimed; drop them so the pool
	// refills elsewhere.
	for i, meta := range metas {
		if meta.Status == StatusWarm && slices.Contains(low, meta.Node) {
			log.Printf("reconcile: %s: warm on low-disk node %q, deleting", meta.ID, meta.Node)
			m.withLock(meta.ID, func(cur store.Meta) error {
				if cur.Status != StatusWarm {
					return nil // claimed since the scan
				}
				return m.remove(ctx, cur)
			})
			metas[i].Status = StatusDeleting
		}
	}

	// 4. Pods follow meta: recreate missing ones, fix annotations, drop strays.
	pods, err := m.Kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "dawnbx/id"})
	if err != nil {
		log.Printf("reconcile: list pods: %v", err)
		return
	}
	have := map[string]*corev1.Pod{}
	for i, p := range pods.Items {
		have[p.Name] = &pods.Items[i]
	}
	want := map[string]bool{}
	for _, meta := range metas {
		if meta.Status == StatusDeleting {
			continue
		}
		want[meta.ID] = true
		m.withLock(meta.ID, func(cur store.Meta) error {
			if p := have[cur.ID]; p != nil {
				var err error
				if cur, err = m.pinAndPersist(cur, p); err != nil {
					return err
				}
			} else if cur.Node == "" && cur.Status != StatusWarm && m.Self != "" {
				// Made before multi-node: the workspace is on this disk.
				cur.Node = m.Self
				if err := m.Store.WriteMeta(cur); err != nil {
					return err
				}
			}
			switch {
			case cur.Status == StatusStopped && have[cur.ID] != nil:
				return m.Kube.CoreV1().Pods(Namespace).Delete(ctx, cur.ID, metav1.DeleteOptions{})
			case cur.Status == StatusStopped:
			case have[cur.ID] == nil:
				log.Printf("reconcile: %s: pod missing, recreating", cur.ID)
				restarted := m.Now().UTC().Format(time.RFC3339)
				if cur.Status == StatusWarm {
					restarted = "" // nobody saw it run yet
				}
				_, err := m.ensurePod(ctx, cur, restarted)
				return err
			default:
				m.syncAnnotations(ctx, cur)
			}
			return nil
		})
	}
	for _, p := range pods.Items {
		// Pods are created only after meta.json, so a labelled pod with no
		// meta is left over from a kill that crashed or a wiped data dir.
		if !want[p.Name] && !m.exists(p.Name) {
			log.Printf("reconcile: %s: pod without meta.json, deleting", p.Name)
			err := m.Kube.CoreV1().Pods(Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				log.Printf("reconcile: delete %s: %v", p.Name, err)
			}
		}
	}
	m.FillPool(ctx)
	if startup {
		log.Printf("reconcile: startup done, %d sandboxes", len(want))
	}
}

// workerDisks measures every Ready worker's data volume in parallel.
func (m *Manager) workerDisks(ctx context.Context) map[string]*nodeDisk {
	disks := map[string]*nodeDisk{}
	nodes, err := m.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("reconcile: list nodes: %v", err)
		return disks
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			ready = ready || (c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue)
		}
		if n.Name == m.Self || !ready {
			continue // a NotReady node would only time out
		}
		wg.Go(func() {
			if d := m.diskOf(ctx, n.Name); d != nil {
				mu.Lock()
				disks[n.Name] = d
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return disks
}

func (m *Manager) exists(id string) bool {
	_, err := os.Stat(m.Store.Dir(id))
	return err == nil
}

// withLock re-reads meta under the sandbox lock so the reaper never acts on
// state an API call changed since the scan.
func (m *Manager) withLock(id string, fn func(store.Meta) error) {
	unlock := m.lock(id)
	defer unlock()
	cur, err := m.Store.ReadMeta(id)
	if err != nil {
		return
	}
	if err := fn(cur); err != nil {
		log.Printf("reconcile: %s: %v", id, err)
	}
}
