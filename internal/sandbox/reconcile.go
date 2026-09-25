package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

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
		usage[meta.ID] = DiskUsage(m.Store.WS(meta.ID))
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

	// 3. Volume headroom (R11/R17): below 10% free, delete expiring sandboxes
	// largest first, then stop the largest keep-forever one.
	if free, err := FreePct(m.Store.Root); err == nil && free < 10 {
		sort.Slice(metas, func(i, j int) bool { return usage[metas[i].ID] > usage[metas[j].ID] })
		for _, meta := range metas {
			if free, _ = FreePct(m.Store.Root); free >= 10 {
				break
			}
			if meta.ExpiresAt != nil {
				log.Printf("reconcile: volume %.1f%% free, deleting %s", free, meta.ID)
				m.withLock(meta.ID, func(cur store.Meta) error { return m.remove(ctx, cur) })
				meta.Status = StatusDeleting
			}
		}
		if free, _ = FreePct(m.Store.Root); free < 10 {
			for i, meta := range metas {
				if meta.ExpiresAt == nil && meta.Status == StatusRunning {
					log.Printf("reconcile: volume %.1f%% free, stopping %s", free, meta.ID)
					m.withLock(meta.ID, func(cur store.Meta) error { return m.stop(ctx, cur, "disk_full") })
					metas[i].Status = StatusStopped
					break
				}
			}
		}
	}

	// 4. Pods follow meta: recreate missing ones, fix annotations, drop strays.
	pods, err := m.Kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "dawnbx/id"})
	if err != nil {
		log.Printf("reconcile: list pods: %v", err)
		return
	}
	have := map[string]bool{}
	for _, p := range pods.Items {
		have[p.Name] = true
	}
	want := map[string]bool{}
	for _, meta := range metas {
		if meta.Status == StatusDeleting {
			continue
		}
		want[meta.ID] = true
		m.withLock(meta.ID, func(cur store.Meta) error {
			switch {
			case cur.Status == StatusStopped && have[cur.ID]:
				return m.Kube.CoreV1().Pods(Namespace).Delete(ctx, cur.ID, metav1.DeleteOptions{})
			case cur.Status == StatusStopped:
			case !have[cur.ID]:
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
