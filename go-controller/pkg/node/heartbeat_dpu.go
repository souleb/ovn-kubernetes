package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// defaultLeaseDurationSeconds is the default duration of the lease in seconds.
	// This the default value for corev1.Node leases
	defaultLeaseDurationSeconds = 40
	// defaultLeaseNS is the default namespace for the lease.
	defaultLeaseNS = "dpu-lease-zone"
)

type heartbeatOptions struct {
	holderIdentity       string
	leaseDurationSeconds int32
	leaseNS              string
	mode                 string
	interval             time.Duration
}

type HeartbeatOption interface {
	Apply(*heartbeatOptions)
}

type HolderIdentityOption string

func (o HolderIdentityOption) Apply(options *heartbeatOptions) {
	options.holderIdentity = string(o)
}

type LeaseDurationSecondsOption int32

func (o LeaseDurationSecondsOption) Apply(options *heartbeatOptions) {
	options.leaseDurationSeconds = int32(o)
}

type LeaseNSOption string

func (o LeaseNSOption) Apply(options *heartbeatOptions) {
	options.leaseNS = string(o)
}

type ModeOption string

func (o ModeOption) Apply(options *heartbeatOptions) {
	options.mode = string(o)
}

type IntervalOption time.Duration

func (o IntervalOption) Apply(options *heartbeatOptions) {
	options.interval = time.Duration(o)
}

type heartbeat struct {
	nodeName string
	client   kubernetes.Interface
	lease    *coordinationv1.Lease
	errChan  chan error
	heartbeatOptions
}

func newHeartbeat(client kubernetes.Interface, nodeName string, errChan chan error, opts ...HeartbeatOption) *heartbeat {
	o := &heartbeatOptions{}
	for _, opt := range opts {
		opt.Apply(o)
	}

	if o.leaseDurationSeconds == 0 {
		o.leaseDurationSeconds = defaultLeaseDurationSeconds
	}
	if o.leaseNS == "" {
		o.leaseNS = defaultLeaseNS
	}
	if o.interval == 0 {
		// default interval is 10 seconds
		o.interval = 10 * time.Second
	}
	if o.mode == "" {
		o.mode = types.NodeModeDPU
	}
	return &heartbeat{
		nodeName:         nodeName,
		client:           client,
		errChan:          errChan,
		heartbeatOptions: *o,
	}
}

func (h *heartbeat) run(ctx context.Context) error {
	switch h.mode {
	case types.NodeModeDPU:
		return h.runDPUNode(ctx)
	case types.NodeModeDPUHost:
		return h.runDPUHost(ctx)
	default:
		return fmt.Errorf("unknown node mode: %s", h.mode)
	}
}

func (h *heartbeat) runDPUNode(ctx context.Context) error {
	// check if lease exist
	lease, err := h.get(ctx)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

	}
	// if lease exit adopt and update
	if lease != nil {
		h.lease = lease
		if err = h.update(ctx, h.createLeaseSpec(h.lease.Spec.AcquireTime.Time, time.Now())); err != nil {
			return err
		}
	} else {
		// otherwise create it
		t := time.Now()
		if err = h.create(ctx, h.createLeaseSpec(t, t)); err != nil {
			return err
		}
	}

	go func() {
		ticker := newTicker(h.interval)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				// release the lease
				err := h.client.CoordinationV1().Leases(h.leaseNS).Delete(ctx, h.nodeName, metav1.DeleteOptions{})
				h.errChan <- err
				return
			case <-ticker.C:
				if err = h.update(ctx, h.createLeaseSpec(h.lease.Spec.AcquireTime.Time, time.Now())); err != nil {
					klog.Errorf("Failed to update node lease for heartbeat: %v", err)
					h.errChan <- err
				}
			}
		}
	}()

	return nil
}

func (h *heartbeat) runDPUHost(ctx context.Context) error {
	go func() {
		ticker := newTicker(h.interval)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				h.errChan <- nil
				return
			case <-ticker.C:
				if valid, err := isHeartBeatValid(ctx, h.client, h.leaseNS); err != nil || !valid {
					klog.Errorf("Heartbeat lease is not valid: %v", err)
					h.errChan <- err
				}
			}
		}
	}()

	return nil
}

func (h *heartbeat) get(ctx context.Context) (*coordinationv1.Lease, error) {
	lease, err := h.client.CoordinationV1().Leases(h.leaseNS).Get(ctx, h.nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (h *heartbeat) update(ctx context.Context, leaseSpec coordinationv1.LeaseSpec) error {
	if h.lease == nil {
		return errors.New("lease not initialized, call get or create first")
	}

	h.lease.Spec = leaseSpec

	lease, err := h.client.CoordinationV1().Leases(h.leaseNS).Update(ctx, h.lease, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	h.lease = lease
	return nil
}

func (h *heartbeat) create(ctx context.Context, leaseSpec coordinationv1.LeaseSpec) error {
	var err error
	h.lease, err = h.client.CoordinationV1().Leases(h.leaseNS).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.nodeName,
			Namespace: h.leaseNS,
		},
		Spec: leaseSpec,
	}, metav1.CreateOptions{})
	return err
}

func (h *heartbeat) createLeaseSpec(acquireTime, renewTime time.Time) coordinationv1.LeaseSpec {
	return coordinationv1.LeaseSpec{
		HolderIdentity:       &h.holderIdentity,
		LeaseDurationSeconds: &h.leaseDurationSeconds,
		AcquireTime:          &metav1.MicroTime{Time: acquireTime},
		RenewTime:            &metav1.MicroTime{Time: renewTime},
	}
}

// isHeartBeatValid checks if there are any leases in the given namespace.
// If there are no leases, or if any lease is expired, it returns false.
// If all leases are valid, it returns true.
func isHeartBeatValid(ctx context.Context, client kubernetes.Interface, ns string) (bool, error) {
	leases, err := client.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}

	if len(leases.Items) == 0 {
		return false, fmt.Errorf("no lease found in namespace %s", ns)
	}

	for _, lease := range leases.Items {
		if lease.Spec.RenewTime.Time.Add(time.Second * time.Duration(*lease.Spec.LeaseDurationSeconds)).Before(time.Now()) {
			return false, fmt.Errorf("lease %s is expired", lease.Name)
		}
	}

	return true, nil
}

func newTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}
