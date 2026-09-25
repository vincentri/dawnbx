package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/store"
)

const MaxFork = 10

// CopyTree copies src's contents into the existing dir dst. cp -a keeps
// symlinks as symlinks, so links planted in /workspace never make the server
// read host files. Replaced in tests.
var CopyTree = func(src, dst string) error {
	out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy workspace: %v: %s", err, out)
	}
	return nil
}

type ForkReq struct {
	Count  int     `json:"count"`
	TTL    *string `json:"ttl"`
	ttlSet bool
}

func (r *ForkReq) HasTTL() { r.ttlSet = true }

// Fork copies the parent's /workspace into count new sandboxes. The parent's
// processes are frozen during the copy so files are consistent; children
// start with fresh processes (R3).
func (m *Manager) Fork(ctx context.Context, id string, r ForkReq) ([]*View, error) {
	if r.Count == 0 {
		r.Count = 1
	}
	if r.Count < 0 || r.Count > MaxFork {
		return nil, errf(400, "invalid_request", fmt.Sprintf("fork 1 to %d at a time", MaxFork), "bad count %d", r.Count)
	}
	unlock := m.lock(id)
	defer unlock()
	parent, err := m.running(id)
	if err != nil {
		return nil, err
	}
	// ponytail: checks % free, not parent size x count; the 30 s reaper catches overshoot.
	if err := m.checkHeadroom(15); err != nil {
		return nil, err
	}

	now := m.Now().UTC()
	var exp *time.Time
	if r.TTL != nil {
		d, e := parseTTL(*r.TTL)
		if e != nil {
			return nil, e
		}
		t := now.Add(d)
		exp = &t
	} else if !r.ttlSet {
		t := now.Add(DefaultTTL)
		exp = &t
	}

	// Warm sandboxes are claimed before freezing so the parent stays paused only for the copy.
	// Kids live on the parent's node: the copy is disk to disk.
	remote := !m.local(parent)
	node := parent.Node
	if !remote {
		node = m.Self
	} else if slices.Contains(m.lowDisk(), node) {
		return nil, errf(507, "disk_low", "kill unused sandboxes on that node (dawnbx ls) or grow its disk", "node %s is under 15%% free; forks live on their parent's node", node)
	}
	kids := make([]store.Meta, r.Count)
	pods := make([]*corev1.Pod, r.Count)
	for i := range kids {
		want := store.Meta{Image: parent.Image, Parent: parent.ID, Created: now, ExpiresAt: exp,
			Status: StatusRunning, Network: parent.Network, CPU: parent.CPU, Memory: parent.Memory,
			Org: parent.Org, KeyID: parent.KeyID, Node: node}
		got, p, unlock, ok := m.claim(ctx, want)
		if !ok {
			// Held until the pod exists so the reaper can't start one mid-copy.
			got.ID = store.NewID()
			unlock = m.lock(got.ID)
		}
		kids[i], pods[i] = got, p
		defer unlock()
	}
	cleanup := func() {
		for _, k := range kids {
			m.Kube.CoreV1().Pods(Namespace).Delete(context.Background(), k.ID, metav1.DeleteOptions{})
			os.RemoveAll(m.Store.Dir(k.ID))
			if remote {
				go m.onNode(context.Background(), node, "rm-"+k.ID, "rm -rf -- /sb/"+k.ID)
			}
		}
	}

	if remote {
		// The copy goes through exec, so the kids must be up before it.
		for i, k := range kids {
			if pods[i] == nil {
				if err := m.Store.Create(k); err != nil {
					cleanup()
					return nil, err
				}
			}
		}
		views, err := m.startKids(ctx, kids, pods)
		if err == nil {
			err = m.freezeAndCopy(ctx, parent.ID, kids, pods, true)
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		return views, nil
	}
	if err := m.freezeAndCopy(ctx, parent.ID, kids, pods, false); err != nil {
		cleanup()
		return nil, err
	}
	views, err := m.startKids(ctx, kids, pods)
	if err != nil {
		cleanup()
		return nil, err
	}
	return views, nil
}

// startKids starts the pods of unclaimed kids and waits until all are Ready.
func (m *Manager) startKids(ctx context.Context, kids []store.Meta, pods []*corev1.Pod) ([]*View, error) {

	views := make([]*View, len(kids))
	errs := make([]error, len(kids))
	var wg sync.WaitGroup
	for i, k := range kids {
		if pods[i] != nil {
			views[i] = view(k, pods[i])
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, errs[i] = m.ensurePod(ctx, k, ""); errs[i] != nil {
				return
			}
			p, err := m.waitReady(ctx, k.ID)
			views[i], errs[i] = view(k, p), err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return views, nil
}

// freezeAndCopy copies the parent's workspace into each kid. Claimed kids
// (pods[i] != nil) already have a dir and a running pod. remote kids are all
// running already and get the copy streamed through exec.
func (m *Manager) freezeAndCopy(ctx context.Context, parent string, kids []store.Meta, pods []*corev1.Pod, remote bool) error {
	// kill -1 skips pid 1 (the sleep keeping the pod up) and the calling shell.
	if _, err := m.RunExec(ctx, parent, []string{"sh", "-c", "kill -STOP -1 2>/dev/null; sync"}, nil, io.Discard, io.Discard); err != nil {
		return err
	}
	defer func() {
		// Resume even if the request was cancelled mid-copy.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.RunExec(cctx, parent, []string{"sh", "-c", "kill -CONT -1 2>/dev/null; true"}, nil, io.Discard, io.Discard)
	}()
	for i, k := range kids {
		if remote {
			if err := m.streamCopy(ctx, parent, k.ID); err != nil {
				return err
			}
			continue
		}
		if pods[i] == nil {
			if err := m.Store.Create(k); err != nil {
				return err
			}
		}
		if err := CopyTree(m.Store.WS(parent), m.Store.WS(k.ID)); err != nil {
			return errf(500, "fork_failed", "check free space on the data volume", "%v", err)
		}
	}
	return nil
}
