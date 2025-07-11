// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/hashicorp/go-multierror"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// batchUpdateInterval is how long we wait to batch updates
	batchUpdateInterval = 50 * time.Millisecond

	// maxParallelRequestsPerDerive  is the maximum number of parallel Vault
	// create token requests that may be outstanding per derive request
	maxParallelRequestsPerDerive = 16

	// NodeDrainEvents are the various drain messages
	NodeDrainEventDrainSet      = "Node drain strategy set"
	NodeDrainEventDrainDisabled = "Node drain disabled"
	NodeDrainEventDrainUpdated  = "Node drain strategy updated"

	// NodeEligibilityEventEligible is used when the nodes eligiblity is marked
	// eligible
	NodeEligibilityEventEligible = "Node marked as eligible for scheduling"

	// NodeEligibilityEventIneligible is used when the nodes eligiblity is marked
	// ineligible
	NodeEligibilityEventIneligible = "Node marked as ineligible for scheduling"

	// NodeHeartbeatEventReregistered is the message used when the node becomes
	// reregistered by the heartbeat.
	NodeHeartbeatEventReregistered = "Node reregistered by heartbeat"

	// NodeWaitingForNodePool is the message used when the node is waiting for
	// its node pool to be created.
	NodeWaitingForNodePool = "Node registered but waiting for node pool to be created"
)

// Node endpoint is used for client interactions
type Node struct {
	srv    *Server
	logger hclog.Logger

	// ctx provides context regarding the underlying connection
	ctx *RPCContext

	// updates holds pending client status updates for allocations
	updates []*structs.Allocation

	// evals holds pending rescheduling eval updates triggered by failed allocations
	evals []*structs.Evaluation

	// updateFuture is used to wait for the pending batch update
	// to complete. This may be nil if no batch is pending.
	updateFuture *structs.BatchFuture

	// updateTimer is the timer that will trigger the next batch
	// update, and may be nil if there is no batch pending.
	updateTimer *time.Timer

	// updatesLock synchronizes access to the updates list,
	// the future and the timer.
	updatesLock sync.Mutex
}

func NewNodeEndpoint(srv *Server, ctx *RPCContext) *Node {
	return &Node{
		srv:     srv,
		ctx:     ctx,
		logger:  srv.logger.Named("client"),
		updates: []*structs.Allocation{},
		evals:   []*structs.Evaluation{},
	}
}

// Register is used to upsert a client that is available for scheduling
func (n *Node) Register(args *structs.NodeRegisterRequest, reply *structs.NodeUpdateResponse) error {
	// note that we trust-on-first use and the identity will be anonymous for
	// that initial request; we lean on mTLS for handling that safely
	authErr := n.srv.Authenticate(n.ctx, args)

	isForwarded := args.IsForwarded()
	if done, err := n.srv.forward("Node.Register", args, args, reply); done {
		// We have a valid node connection since there is no error from the
		// forwarded server, so add the mapping to cache the
		// connection and allow the server to send RPCs to the client.
		if err == nil && n.ctx != nil && n.ctx.NodeID == "" && !isForwarded {
			n.ctx.NodeID = args.Node.ID
			n.srv.addNodeConn(n.ctx)
		}

		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "client", "register"}, time.Now())

	// Validate the arguments
	if args.Node == nil {
		return fmt.Errorf("missing node for client registration")
	}
	if args.Node.ID == "" {
		return fmt.Errorf("missing node ID for client registration")
	}
	if args.Node.Datacenter == "" {
		return fmt.Errorf("missing datacenter for client registration")
	}
	if args.Node.Name == "" {
		return fmt.Errorf("missing node name for client registration")
	}
	if len(args.Node.Attributes) == 0 {
		return fmt.Errorf("missing attributes for client registration")
	}
	if args.Node.SecretID == "" {
		return fmt.Errorf("missing node secret ID for client registration")
	}
	if args.Node.NodePool != "" {
		err := structs.ValidateNodePoolName(args.Node.NodePool)
		if err != nil {
			return fmt.Errorf("invalid node pool: %v", err)
		}
		if args.Node.NodePool == structs.NodePoolAll {
			return fmt.Errorf("node is not allowed to register in node pool %q", structs.NodePoolAll)
		}
	}

	// Default the status if none is given
	if args.Node.Status == "" {
		args.Node.Status = structs.NodeStatusInit
	}
	if !structs.ValidNodeStatus(args.Node.Status) {
		return fmt.Errorf("invalid status for node")
	}

	// Default to eligible for scheduling if unset
	if args.Node.SchedulingEligibility == "" {
		args.Node.SchedulingEligibility = structs.NodeSchedulingEligible
	}

	// Default the node pool if none is given.
	if args.Node.NodePool == "" {
		args.Node.NodePool = structs.NodePoolDefault
	}

	// Set the timestamp when the node is registered
	args.Node.StatusUpdatedAt = time.Now().Unix()

	// Compute the node class
	if err := args.Node.ComputeClass(); err != nil {
		return fmt.Errorf("failed to computed node class: %v", err)
	}

	// Look for the node so we can detect a state transition
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}

	ws := memdb.NewWatchSet()
	originalNode, err := snap.NodeByID(ws, args.Node.ID)
	if err != nil {
		return err
	}

	if originalNode != nil {
		// Check if the SecretID has been tampered with
		if args.Node.SecretID != originalNode.SecretID && originalNode.SecretID != "" {
			return fmt.Errorf("node secret ID does not match. Not registering node.")
		}

		// Don't allow the Register method to update the node status. Only the
		// UpdateStatus method should be able to do this.
		if originalNode.Status != "" {
			args.Node.Status = originalNode.Status
		}
	}

	// We have a valid node connection, so add the mapping to cache the
	// connection and allow the server to send RPCs to the client. We only cache
	// the connection if it is not being forwarded from another server.
	if n.ctx != nil && n.ctx.NodeID == "" && !args.IsForwarded() {
		n.ctx.NodeID = args.Node.ID
		n.srv.addNodeConn(n.ctx)
	}

	// Commit this update via Raft.
	//
	// Only the authoritative region is allowed to create the node pool for the
	// node if it doesn't exist yet. This prevents non-authoritative regions
	// from having to push their local state to the authoritative region.
	//
	// Nodes in non-authoritative regions that are registered with a new node
	// pool are kept in the `initializing` status until the node pool is
	// created and replicated.
	if n.srv.Region() == n.srv.config.AuthoritativeRegion {
		args.CreateNodePool = true
	}
	_, index, err := n.srv.raftApply(structs.NodeRegisterRequestType, args)
	if err != nil {
		n.logger.Error("register failed", "error", err)
		return err
	}
	reply.NodeModifyIndex = index

	// Check if we should trigger evaluations
	if shouldCreateNodeEval(originalNode, args.Node) {
		evalIDs, evalIndex, err := n.createNodeEvals(args.Node, index)
		if err != nil {
			n.logger.Error("eval creation failed", "error", err)
			return err
		}
		reply.EvalIDs = evalIDs
		reply.EvalCreateIndex = evalIndex
	}

	// Check if we need to setup a heartbeat
	if !args.Node.TerminalStatus() {
		ttl, err := n.srv.resetHeartbeatTimer(args.Node.ID)
		if err != nil {
			n.logger.Error("heartbeat reset failed", "error", err)
			return err
		}
		reply.HeartbeatTTL = ttl
	}

	// Set the reply index
	reply.Index = index
	snap, err = n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}

	n.srv.peerLock.RLock()
	defer n.srv.peerLock.RUnlock()
	if err := n.constructNodeServerInfoResponse(args.Node.ID, snap, reply); err != nil {
		n.logger.Error("failed to populate NodeUpdateResponse", "error", err)
		return err
	}

	return nil
}

// shouldCreateNodeEval returns true if the node update may result into
// allocation updates, so the node should be re-evaluating.
//
// Such cases might be:
// * node health/drain status changes that may result into alloc rescheduling
// * node drivers or attributes changing that may cause system job placement changes
func shouldCreateNodeEval(original, updated *structs.Node) bool {
	if structs.ShouldDrainNode(updated.Status) {
		return true
	}

	if original == nil {
		return nodeStatusTransitionRequiresEval(updated.Status, structs.NodeStatusInit)
	}

	if nodeStatusTransitionRequiresEval(updated.Status, original.Status) {
		return true
	}

	// check fields used by the feasibility checks in ../scheduler/feasible.go,
	// whether through a Constraint explicitly added by user or an implicit constraint
	// added through a driver/volume check.
	//
	// Node Resources (e.g. CPU/Memory) are handled differently, using blocked evals,
	// and not relevant in this check.
	return !(original.ID == updated.ID &&
		original.Datacenter == updated.Datacenter &&
		original.Name == updated.Name &&
		original.NodeClass == updated.NodeClass &&
		reflect.DeepEqual(original.Attributes, updated.Attributes) &&
		reflect.DeepEqual(original.Meta, updated.Meta) &&
		reflect.DeepEqual(original.Drivers, updated.Drivers) &&
		reflect.DeepEqual(original.HostVolumes, updated.HostVolumes) &&
		equalDevices(original, updated))
}

func equalDevices(n1, n2 *structs.Node) bool {
	// ignore super old nodes, mostly to avoid nil dereferencing
	if n1.NodeResources == nil || n2.NodeResources == nil {
		return n1.NodeResources == n2.NodeResources
	}

	// treat nil and empty value as equal
	if len(n1.NodeResources.Devices) == 0 {
		return len(n1.NodeResources.Devices) == len(n2.NodeResources.Devices)
	}

	return reflect.DeepEqual(n1.NodeResources.Devices, n2.NodeResources.Devices)
}

// constructNodeServerInfoResponse assumes the n.srv.peerLock is held for reading.
func (n *Node) constructNodeServerInfoResponse(nodeID string, snap *state.StateSnapshot, reply *structs.NodeUpdateResponse) error {
	leaderAddr, _ := n.srv.raft.LeaderWithID()
	reply.LeaderRPCAddr = string(leaderAddr)

	// Reply with config information required for future RPC requests
	reply.Servers = make([]*structs.NodeServerInfo, 0, len(n.srv.localPeers))
	for _, v := range n.srv.localPeers {
		reply.Servers = append(reply.Servers,
			&structs.NodeServerInfo{
				RPCAdvertiseAddr: v.RPCAddr.String(),
				Datacenter:       v.Datacenter,
			})
	}

	ws := memdb.NewWatchSet()

	// Add ClientStatus information to heartbeat response.
	if node, err := snap.NodeByID(ws, nodeID); err == nil && node != nil {
		reply.SchedulingEligibility = node.SchedulingEligibility
	} else if node == nil {

		// If the node is not found, leave reply.SchedulingEligibility as
		// the empty string. The response handler in the client treats this
		// as a no-op. As there is no call to action for an operator, log it
		// at debug level.
		n.logger.Debug("constructNodeServerInfoResponse: node not found",
			"node_id", nodeID)
	} else {

		// This case is likely only reached via a code error in state store
		return err
	}

	// TODO(sean@): Use an indexed node count instead
	//
	// Snapshot is used only to iterate over all nodes to create a node
	// count to send back to Nomad Clients in their heartbeat so Clients
	// can estimate the size of the cluster.
	iter, err := snap.Nodes(ws)
	if err == nil {
		for {
			raw := iter.Next()
			if raw == nil {
				break
			}
			reply.NumNodes++
		}
	}

	reply.Features = n.srv.EnterpriseState.Features()

	return nil
}

// Deregister is used to remove a client from the cluster. If a client should
// just be made unavailable for scheduling, a status update is preferred.
func (n *Node) Deregister(args *structs.NodeDeregisterRequest, reply *structs.NodeUpdateResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.Deregister", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "deregister"}, time.Now())

	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return structs.ErrPermissionDenied
	} else if !aclObj.AllowNodeWrite() {
		return structs.ErrPermissionDenied
	}

	if args.NodeID == "" {
		return fmt.Errorf("missing node ID for client deregistration")
	}

	// deregister takes a batch
	repack := &structs.NodeBatchDeregisterRequest{
		NodeIDs:      []string{args.NodeID},
		WriteRequest: args.WriteRequest,
	}

	return n.deregister(repack, reply, func() (interface{}, uint64, error) {
		return n.srv.raftApply(structs.NodeDeregisterRequestType, args)
	})
}

// BatchDeregister is used to remove client nodes from the cluster.
func (n *Node) BatchDeregister(args *structs.NodeBatchDeregisterRequest, reply *structs.NodeUpdateResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.BatchDeregister", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "batch_deregister"}, time.Now())

	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return structs.ErrPermissionDenied
	} else if !aclObj.AllowNodeWrite() {
		return structs.ErrPermissionDenied
	}

	if len(args.NodeIDs) == 0 {
		return fmt.Errorf("missing node IDs for client deregistration")
	}

	return n.deregister(args, reply, func() (interface{}, uint64, error) {
		return n.srv.raftApply(structs.NodeBatchDeregisterRequestType, args)
	})
}

// deregister takes a raftMessage closure, to support both Deregister and
// BatchDeregister. The caller should have already authorized the request.
func (n *Node) deregister(args *structs.NodeBatchDeregisterRequest,
	reply *structs.NodeUpdateResponse,
	raftApplyFn func() (interface{}, uint64, error),
) error {
	// Look for the node
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}

	nodes := make([]*structs.Node, 0, len(args.NodeIDs))
	for _, nodeID := range args.NodeIDs {
		node, err := snap.NodeByID(nil, nodeID)
		if err != nil {
			return err
		}
		if node == nil {
			return fmt.Errorf("node not found")
		}
		nodes = append(nodes, node)
	}

	// Commit this update via Raft
	_, index, err := raftApplyFn()
	if err != nil {
		n.logger.Error("raft message failed", "error", err)
		return err
	}

	for _, node := range nodes {
		nodeID := node.ID

		// Clear the heartbeat timer if any
		n.srv.clearHeartbeatTimer(nodeID)

		// Create the evaluations for this node
		evalIDs, evalIndex, err := n.createNodeEvals(node, index)
		if err != nil {
			n.logger.Error("eval creation failed", "error", err)
			return err
		}

		reply.EvalIDs = append(reply.EvalIDs, evalIDs...)
		// Set the reply eval create index just the first time
		if reply.EvalCreateIndex == 0 {
			reply.EvalCreateIndex = evalIndex
		}
	}

	reply.NodeModifyIndex = index
	reply.Index = index
	return nil
}

// UpdateStatus is used to update the status of a client node.
//
// Clients with non-terminal allocations must first call UpdateAlloc to be able
// to transition from the initializing status to ready.
//
// Clients node pool must exist for them to be able to transition from
// initializing to ready.
//
//	                ┌────────────────────────────────────── No ───┐
//	                │                                             │
//	             ┌──▼───┐          ┌─────────────┐       ┌────────┴────────┐
//	── Register ─► init ├─ ready ──► Has allocs? ├─ Yes ─► Allocs updated? │
//	             └──▲──▲┘          └─────┬───────┘       └────────┬────────┘
//	                │  │                 │                        │
//	                │  │                 └─ No ─┐  ┌─────── Yes ──┘
//	                │  │                        │  │
//	                │  │               ┌────────▼──▼───────┐
//	                │  └──────────No───┤ Node pool exists? │
//	                │                  └─────────┬─────────┘
//	                │                            │
//	              ready                         Yes
//	                │                            │
//	         ┌──────┴───────┐                ┌───▼───┐         ┌──────┐
//	         │ disconnected ◄─ disconnected ─┤ ready ├─ down ──► down │
//	         └──────────────┘                └───▲───┘         └──┬───┘
//	                                             │                │
//	                                             └──── ready ─────┘
func (n *Node) UpdateStatus(args *structs.NodeUpdateStatusRequest, reply *structs.NodeUpdateResponse) error {
	// UpdateStatus receives requests from client and servers that mark failed
	// heartbeats, so we can't use AuthenticateClientOnly
	authErr := n.srv.Authenticate(n.ctx, args)

	isForwarded := args.IsForwarded()
	if done, err := n.srv.forward("Node.UpdateStatus", args, args, reply); done {
		// We have a valid node connection since there is no error from the
		// forwarded server, so add the mapping to cache the
		// connection and allow the server to send RPCs to the client.
		if err == nil && n.ctx != nil && n.ctx.NodeID == "" && !isForwarded {
			n.ctx.NodeID = args.NodeID
			n.srv.addNodeConn(n.ctx)
		}

		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "client", "update_status"}, time.Now())

	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return structs.ErrPermissionDenied
	} else if !(aclObj.AllowClientOp() || aclObj.AllowServerOp()) {
		return structs.ErrPermissionDenied
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID for client status update")
	}
	if !structs.ValidNodeStatus(args.Status) {
		return fmt.Errorf("invalid status for node")
	}

	// Look for the node
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}

	ws := memdb.NewWatchSet()
	node, err := snap.NodeByID(ws, args.NodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("node not found")
	}

	// We have a valid node connection, so add the mapping to cache the
	// connection and allow the server to send RPCs to the client. We only cache
	// the connection if it is not being forwarded from another server.
	if n.ctx != nil && n.ctx.NodeID == "" && !args.IsForwarded() {
		n.ctx.NodeID = args.NodeID
		n.srv.addNodeConn(n.ctx)
	}

	// XXX: Could use the SecretID here but have to update the heartbeat system
	// to track SecretIDs.

	// Update the timestamp of when the node status was updated
	args.UpdatedAt = time.Now().Unix()

	// Compute next status.
	switch node.Status {
	case structs.NodeStatusInit:
		if args.Status == structs.NodeStatusReady {
			// Keep node in the initializing status if it has allocations but
			// they are not updated.
			allocs, err := snap.AllocsByNodeTerminal(ws, args.NodeID, false)
			if err != nil {
				return fmt.Errorf("failed to query node allocs: %v", err)
			}

			allocsUpdated := node.LastAllocUpdateIndex > node.LastMissedHeartbeatIndex
			if len(allocs) > 0 && !allocsUpdated {
				n.logger.Debug(fmt.Sprintf("marking node as %s due to outdated allocation information", structs.NodeStatusInit))
				args.Status = structs.NodeStatusInit
			}

			// Keep node in the initialing status if it's in a node pool that
			// doesn't exist.
			pool, err := snap.NodePoolByName(ws, node.NodePool)
			if err != nil {
				return fmt.Errorf("failed to query node pool: %v", err)
			}
			if pool == nil {
				n.logger.Debug(fmt.Sprintf("marking node as %s due to missing node pool", structs.NodeStatusInit))
				args.Status = structs.NodeStatusInit
				if !node.HasEvent(NodeWaitingForNodePool) {
					args.NodeEvent = structs.NewNodeEvent().
						SetSubsystem(structs.NodeEventSubsystemCluster).
						SetMessage(NodeWaitingForNodePool).
						AddDetail("node_pool", node.NodePool)
				}
			}
		}
	case structs.NodeStatusDisconnected:
		if args.Status == structs.NodeStatusReady {
			args.Status = structs.NodeStatusInit
		}
	}

	// Commit this update via Raft
	var index uint64
	if node.Status != args.Status || args.NodeEvent != nil {
		// Attach an event if we are updating the node status to ready when it
		// is down via a heartbeat
		if node.Status == structs.NodeStatusDown && args.NodeEvent == nil {
			args.NodeEvent = structs.NewNodeEvent().
				SetSubsystem(structs.NodeEventSubsystemCluster).
				SetMessage(NodeHeartbeatEventReregistered)
		}

		_, index, err = n.srv.raftApply(structs.NodeUpdateStatusRequestType, args)
		if err != nil {
			n.logger.Error("status update failed", "error", err)
			return err
		}
		reply.NodeModifyIndex = index
	}

	// Check if we should trigger evaluations
	if structs.ShouldDrainNode(args.Status) ||
		nodeStatusTransitionRequiresEval(args.Status, node.Status) {
		evalIDs, evalIndex, err := n.createNodeEvals(node, index)
		if err != nil {
			n.logger.Error("eval creation failed", "error", err)
			return err
		}
		reply.EvalIDs = evalIDs
		reply.EvalCreateIndex = evalIndex
	}

	// Check if we need to setup a heartbeat
	if args.Status != structs.NodeStatusDown {
		ttl, err := n.srv.resetHeartbeatTimer(args.NodeID)
		if err != nil {
			n.logger.Error("heartbeat reset failed", "error", err)
			return err
		}
		reply.HeartbeatTTL = ttl
	}

	// Set the reply index and leader
	reply.Index = index
	n.srv.peerLock.RLock()
	defer n.srv.peerLock.RUnlock()
	if err := n.constructNodeServerInfoResponse(node.GetID(), snap, reply); err != nil {
		n.logger.Error("failed to populate NodeUpdateResponse", "error", err)
		return err
	}

	return nil
}

// nodeStatusTransitionRequiresEval is a helper that takes a nodes new and old status and
// returns whether it has transitioned to ready.
func nodeStatusTransitionRequiresEval(newStatus, oldStatus string) bool {
	initToReady := oldStatus == structs.NodeStatusInit && newStatus == structs.NodeStatusReady
	terminalToReady := oldStatus == structs.NodeStatusDown && newStatus == structs.NodeStatusReady
	disconnectedToOther := oldStatus == structs.NodeStatusDisconnected && newStatus != structs.NodeStatusDisconnected
	otherToDisconnected := oldStatus != structs.NodeStatusDisconnected && newStatus == structs.NodeStatusDisconnected
	return initToReady || terminalToReady || disconnectedToOther || otherToDisconnected
}

// UpdateDrain is used to update the drain mode of a client node
func (n *Node) UpdateDrain(args *structs.NodeUpdateDrainRequest,
	reply *structs.NodeDrainUpdateResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.UpdateDrain", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "update_drain"}, time.Now())

	// Check node write permissions
	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.AllowNodeWrite() &&
		!(aclObj.AllowClientOp() && args.GetIdentity().ClientID == args.NodeID) {
		return structs.ErrPermissionDenied
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID for drain update")
	}
	if args.NodeEvent != nil {
		return fmt.Errorf("node event must not be set")
	}

	// The AuthenticatedIdentity is unexported so won't be written via
	// Raft. Record the identity string so it can be written to LastDrain
	args.UpdatedBy = args.GetIdentity().String()

	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	node, err := snap.NodeByID(nil, args.NodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("node not found")
	}

	now := time.Now().UTC()

	// Update the timestamp of when the node status was updated
	args.UpdatedAt = now.Unix()

	// Setup drain strategy
	if args.DrainStrategy != nil {
		// Mark start time for the drain
		if node.DrainStrategy == nil {
			args.DrainStrategy.StartedAt = now
		} else {
			args.DrainStrategy.StartedAt = node.DrainStrategy.StartedAt
		}

		// Mark the deadline time
		if args.DrainStrategy.Deadline.Nanoseconds() > 0 {
			args.DrainStrategy.ForceDeadline = now.Add(args.DrainStrategy.Deadline)
		}
	}

	// Construct the node event
	args.NodeEvent = structs.NewNodeEvent().SetSubsystem(structs.NodeEventSubsystemDrain)
	if node.DrainStrategy == nil && args.DrainStrategy != nil {
		args.NodeEvent.SetMessage(NodeDrainEventDrainSet)
	} else if node.DrainStrategy != nil && args.DrainStrategy != nil {
		args.NodeEvent.SetMessage(NodeDrainEventDrainUpdated)
	} else if node.DrainStrategy != nil && args.DrainStrategy == nil {
		args.NodeEvent.SetMessage(NodeDrainEventDrainDisabled)
	} else {
		args.NodeEvent = nil
	}

	// Commit this update via Raft
	_, index, err := n.srv.raftApply(structs.NodeUpdateDrainRequestType, args)
	if err != nil {
		n.logger.Error("drain update failed", "error", err)
		return err
	}
	reply.NodeModifyIndex = index

	// If the node is transitioning to be eligible, create Node evaluations
	// because there may be a System job registered that should be evaluated.
	if node.SchedulingEligibility == structs.NodeSchedulingIneligible && args.MarkEligible && args.DrainStrategy == nil {
		n.logger.Info("node transitioning to eligible state", "node_id", node.ID)
		evalIDs, evalIndex, err := n.createNodeEvals(node, index)
		if err != nil {
			n.logger.Error("eval creation failed", "error", err)
			return err
		}
		reply.EvalIDs = evalIDs
		reply.EvalCreateIndex = evalIndex
	}

	// Set the reply index
	reply.Index = index
	return nil
}

// UpdateEligibility is used to update the scheduling eligibility of a node
func (n *Node) UpdateEligibility(args *structs.NodeUpdateEligibilityRequest,
	reply *structs.NodeEligibilityUpdateResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.UpdateEligibility", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "update_eligibility"}, time.Now())

	// Check node write permissions
	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.AllowNodeWrite() {
		return structs.ErrPermissionDenied
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID for setting scheduling eligibility")
	}
	if args.NodeEvent != nil {
		return fmt.Errorf("node event must not be set")
	}

	// Check that only allowed types are set
	switch args.Eligibility {
	case structs.NodeSchedulingEligible, structs.NodeSchedulingIneligible:
	default:
		return fmt.Errorf("invalid scheduling eligibility %q", args.Eligibility)
	}

	// Look for the node
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	node, err := snap.NodeByID(nil, args.NodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("node not found")
	}

	if node.DrainStrategy != nil && args.Eligibility == structs.NodeSchedulingEligible {
		return fmt.Errorf("can not set node's scheduling eligibility to eligible while it is draining")
	}

	switch args.Eligibility {
	case structs.NodeSchedulingEligible, structs.NodeSchedulingIneligible:
	default:
		return fmt.Errorf("invalid scheduling eligibility %q", args.Eligibility)
	}

	// Update the timestamp of when the node status was updated
	args.UpdatedAt = time.Now().Unix()

	// Construct the node event
	args.NodeEvent = structs.NewNodeEvent().SetSubsystem(structs.NodeEventSubsystemCluster)
	if node.SchedulingEligibility == args.Eligibility {
		return nil // Nothing to do
	} else if args.Eligibility == structs.NodeSchedulingEligible {
		n.logger.Info("node transitioning to eligible state", "node_id", node.ID)
		args.NodeEvent.SetMessage(NodeEligibilityEventEligible)
	} else {
		n.logger.Info("node transitioning to ineligible state", "node_id", node.ID)
		args.NodeEvent.SetMessage(NodeEligibilityEventIneligible)
	}

	// Commit this update via Raft
	outErr, index, err := n.srv.raftApply(structs.NodeUpdateEligibilityRequestType, args)
	if err != nil {
		n.logger.Error("eligibility update failed", "error", err)
		return err
	}
	if outErr != nil {
		if err, ok := outErr.(error); ok && err != nil {
			n.logger.Error("eligibility update failed", "error", err)
			return err
		}
	}

	// If the node is transitioning to be eligible, create Node evaluations
	// because there may be a System job registered that should be evaluated.
	if node.SchedulingEligibility == structs.NodeSchedulingIneligible && args.Eligibility == structs.NodeSchedulingEligible {
		evalIDs, evalIndex, err := n.createNodeEvals(node, index)
		if err != nil {
			n.logger.Error("eval creation failed", "error", err)
			return err
		}
		reply.EvalIDs = evalIDs
		reply.EvalCreateIndex = evalIndex
	}

	// Set the reply index
	reply.Index = index
	return nil
}

// Evaluate is used to force a re-evaluation of the node
func (n *Node) Evaluate(args *structs.NodeEvaluateRequest, reply *structs.NodeUpdateResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.Evaluate", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "evaluate"}, time.Now())

	// Check node write permissions
	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.AllowNodeWrite() {
		return structs.ErrPermissionDenied
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID for evaluation")
	}

	// Look for the node
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	ws := memdb.NewWatchSet()
	node, err := snap.NodeByID(ws, args.NodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("node not found")
	}

	// Create the evaluation
	evalIDs, evalIndex, err := n.createNodeEvals(node, node.ModifyIndex)
	if err != nil {
		n.logger.Error("eval creation failed", "error", err)
		return err
	}
	reply.EvalIDs = evalIDs
	reply.EvalCreateIndex = evalIndex

	// Set the reply index
	reply.Index = evalIndex

	n.srv.peerLock.RLock()
	defer n.srv.peerLock.RUnlock()
	if err := n.constructNodeServerInfoResponse(node.GetID(), snap, reply); err != nil {
		n.logger.Error("failed to populate NodeUpdateResponse", "error", err)
		return err
	}
	return nil
}

// GetNode is used to request information about a specific node
func (n *Node) GetNode(args *structs.NodeSpecificRequest, reply *structs.SingleNodeResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.GetNode", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "get_node"}, time.Now())

	// Check node read permissions
	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowClientOp() && !aclObj.AllowNodeRead() {
		return structs.ErrPermissionDenied
	}

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Verify the arguments
			if args.NodeID == "" {
				return fmt.Errorf("missing node ID")
			}

			// Look for the node
			out, err := state.NodeByID(ws, args.NodeID)
			if err != nil {
				return err
			}

			// Setup the output
			if out != nil {
				out = out.Sanitize()
				reply.Node = out
				reply.Index = out.ModifyIndex
			} else {
				// Use the last index that affected the nodes table
				index, err := state.Index("nodes")
				if err != nil {
					return err
				}
				reply.Node = nil
				reply.Index = index
			}

			// Set the query response
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// GetAllocs is used to request allocations for a specific node
func (n *Node) GetAllocs(args *structs.NodeSpecificRequest,
	reply *structs.NodeAllocsResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.GetAllocs", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "get_allocs"}, time.Now())

	// Check node read and namespace job read permissions
	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowNodeRead() {
		return structs.ErrPermissionDenied
	}

	// cache namespace perms
	readableNamespaces := map[string]bool{}

	// readNS is a caching namespace read-job helper
	readNS := func(ns string) bool {
		if readable, ok := readableNamespaces[ns]; ok {
			// cache hit
			return readable
		}

		// cache miss
		readable := aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadJob)
		readableNamespaces[ns] = readable
		return readable
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID")
	}

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Look for the node
			allocs, err := state.AllocsByNode(ws, args.NodeID)
			if err != nil {
				return err
			}

			// Setup the output
			if n := len(allocs); n != 0 {
				reply.Allocs = make([]*structs.Allocation, 0, n)
				for _, alloc := range allocs {
					if readNS(alloc.Namespace) {
						reply.Allocs = append(reply.Allocs, alloc)
					}

					// Get the max of all allocs since
					// subsequent requests need to start
					// from the latest index
					reply.Index = maxUint64(reply.Index, alloc.ModifyIndex)
				}
			} else {
				reply.Allocs = nil

				// Use the last index that affected the nodes table
				index, err := state.Index("allocs")
				if err != nil {
					return err
				}

				// Must provide non-zero index to prevent blocking
				// Index 1 is impossible anyways (due to Raft internals)
				if index == 0 {
					reply.Index = 1
				} else {
					reply.Index = index
				}
			}
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// GetClientAllocs is used to request a lightweight list of alloc modify indexes
// per allocation.
func (n *Node) GetClientAllocs(args *structs.NodeSpecificRequest,
	reply *structs.NodeClientAllocsResponse) error {

	// This RPC is only ever called by Nomad clients, so we can use the tightly
	// scoped AuthenticateClientOnly method to authenticate and authorize the
	// request.
	aclObj, authErr := n.srv.AuthenticateClientOnly(n.ctx, args)

	isForwarded := args.IsForwarded()
	if done, err := n.srv.forward("Node.GetClientAllocs", args, args, reply); done {
		// We have a valid node connection since there is no error from the
		// forwarded server, so add the mapping to cache the
		// connection and allow the server to send RPCs to the client.
		if err == nil && n.ctx != nil && n.ctx.NodeID == "" && !isForwarded {
			n.ctx.NodeID = args.NodeID
			n.srv.addNodeConn(n.ctx)
		}

		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "get_client_allocs"}, time.Now())

	if !aclObj.AllowClientOp() {
		return structs.ErrPermissionDenied
	}

	// Verify the arguments
	if args.NodeID == "" {
		return fmt.Errorf("missing node ID")
	}

	// numOldAllocs is used to detect if there is a garbage collection event
	// that effects the node. When an allocation is garbage collected, that does
	// not change the modify index changes and thus the query won't unblock,
	// even though the set of allocations on the node has changed.
	var numOldAllocs int

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Look for the node
			node, err := state.NodeByID(ws, args.NodeID)
			if err != nil {
				return err
			}

			var allocs []*structs.Allocation
			if node != nil {
				if args.SecretID == "" {
					return fmt.Errorf("missing node secret ID for client status update")
				} else if args.SecretID != node.SecretID {
					return fmt.Errorf("node secret ID does not match")
				}

				// We have a valid node connection, so add the mapping to cache the
				// connection and allow the server to send RPCs to the client. We only cache
				// the connection if it is not being forwarded from another server.
				if n.ctx != nil && n.ctx.NodeID == "" && !args.IsForwarded() {
					n.ctx.NodeID = args.NodeID
					n.srv.addNodeConn(n.ctx)
				}

				var err error
				allocs, err = state.AllocsByNode(ws, args.NodeID)
				if err != nil {
					return err
				}
			}

			reply.Allocs = make(map[string]uint64)
			reply.MigrateTokens = make(map[string]string)

			// preferTableIndex is used to determine whether we should build the
			// response index based on the full table indexes versus the modify
			// indexes of the allocations on the specific node. This is
			// preferred in the case that the node doesn't yet have allocations
			// or when we detect a GC that effects the node.
			preferTableIndex := true

			// Setup the output
			if numAllocs := len(allocs); numAllocs != 0 {
				preferTableIndex = false

				for _, alloc := range allocs {
					reply.Allocs[alloc.ID] = alloc.AllocModifyIndex

					// If the allocation is going to do a migration, create a
					// migration token so that the client can authenticate with
					// the node hosting the previous allocation.
					if alloc.ShouldMigrate() {
						prevAllocation, err := state.AllocByID(ws, alloc.PreviousAllocation)
						if err != nil {
							return err
						}

						if prevAllocation != nil && prevAllocation.NodeID != alloc.NodeID {
							allocNode, err := state.NodeByID(ws, prevAllocation.NodeID)
							if err != nil {
								return err
							}
							if allocNode == nil {
								// Node must have been GC'd so skip the token
								continue
							}

							token, err := structs.GenerateMigrateToken(prevAllocation.ID, allocNode.SecretID)
							if err != nil {
								return err
							}
							reply.MigrateTokens[alloc.ID] = token
						}
					}

					reply.Index = maxUint64(reply.Index, alloc.ModifyIndex)
				}

				// Determine if we have less allocations than before. This
				// indicates there was a garbage collection
				if numAllocs < numOldAllocs {
					preferTableIndex = true
				}

				// Store the new number of allocations
				numOldAllocs = numAllocs
			}

			if preferTableIndex {
				// Use the last index that affected the nodes table
				index, err := state.Index("allocs")
				if err != nil {
					return err
				}

				// Must provide non-zero index to prevent blocking
				// Index 1 is impossible anyways (due to Raft internals)
				if index == 0 {
					reply.Index = 1
				} else {
					reply.Index = index
				}
			}
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// UpdateAlloc is used to update the client status of an allocation. It should
// only be called by clients.
//
// Calling this method returns an error when:
//   - The node is not registered in the server yet. Clients must first call the
//     Register method.
//   - The node status is down or disconnected. Clients must call the
//     UpdateStatus method to update its status in the server.
func (n *Node) UpdateAlloc(args *structs.AllocUpdateRequest, reply *structs.GenericResponse) error {
	aclObj, err := n.srv.AuthenticateClientOnly(n.ctx, args)
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if err != nil {
		return structs.ErrPermissionDenied
	}

	if done, err := n.srv.forward("Node.UpdateAlloc", args, args, reply); done {
		return err
	}

	defer metrics.MeasureSince([]string{"nomad", "client", "update_alloc"}, time.Now())
	if !aclObj.AllowClientOp() {
		return structs.ErrPermissionDenied
	}

	// Ensure at least a single alloc
	if len(args.Alloc) == 0 {
		return fmt.Errorf("must update at least one allocation")
	}

	// Ensure the node is allowed to update allocs.
	// The node needs to successfully heartbeat before updating its allocs.
	nodeID := args.Alloc[0].NodeID
	if nodeID == "" {
		return fmt.Errorf("missing node ID")
	}

	node, err := n.srv.State().NodeByID(nil, nodeID)
	if err != nil {
		return fmt.Errorf("failed to retrieve node %s: %v", nodeID, err)
	}
	if node == nil {
		return fmt.Errorf("node %s not found", nodeID)
	}
	if node.UnresponsiveStatus() {
		return fmt.Errorf("node %s is not allowed to update allocs while in status %s", nodeID, node.Status)
	}

	// Ensure that evals aren't set from client RPCs
	// We create them here before the raft update
	if len(args.Evals) != 0 {
		return fmt.Errorf("evals field must not be set")
	}

	// Update modified timestamp for client initiated allocation updates
	now := time.Now()
	var evals []*structs.Evaluation

	for _, allocToUpdate := range args.Alloc {
		evalTriggerBy := ""
		allocToUpdate.ModifyTime = now.UTC().UnixNano()

		alloc, _ := n.srv.State().AllocByID(nil, allocToUpdate.ID)
		if alloc == nil {
			continue
		}

		if !allocToUpdate.TerminalStatus() && alloc.ClientStatus != structs.AllocClientStatusUnknown {
			continue
		}

		var job *structs.Job
		var jobType string
		var jobPriority int

		job, err = n.srv.State().JobByID(nil, alloc.Namespace, alloc.JobID)
		if err != nil {
			n.logger.Debug("UpdateAlloc unable to find job", "job", alloc.JobID, "error", err)
			continue
		}

		// If the job is nil it means it has been de-registered.
		if job == nil {
			jobType = alloc.Job.Type
			jobPriority = alloc.Job.Priority
			evalTriggerBy = structs.EvalTriggerJobDeregister
			allocToUpdate.DesiredStatus = structs.AllocDesiredStatusStop
			n.logger.Debug("UpdateAlloc unable to find job - shutting down alloc", "job", alloc.JobID)
		}

		var taskGroup *structs.TaskGroup
		if job != nil {
			jobType = job.Type
			jobPriority = job.Priority
			taskGroup = job.LookupTaskGroup(alloc.TaskGroup)
		}

		// Add an evaluation if this is a failed alloc that is currently
		// eligible for rescheduling
		if evalTriggerBy != structs.EvalTriggerJobDeregister &&
			allocToUpdate.ClientStatus == structs.AllocClientStatusFailed &&
			alloc.FollowupEvalID == "" {

			// If we cannot find the task group for a failed alloc we cannot
			// continue, unless it is an orphan.
			if taskGroup == nil {
				n.logger.Debug("UpdateAlloc unable to find task group for job", "job", alloc.JobID, "alloc", alloc.ID, "task_group", alloc.TaskGroup)
				continue
			}

			// Set trigger by failed if not an orphan.
			if alloc.RescheduleEligible(taskGroup.ReschedulePolicy, now) {
				evalTriggerBy = structs.EvalTriggerRetryFailedAlloc
			}
		}

		var eval *structs.Evaluation
		// If unknown, and not an orphan, set the trigger by.
		if evalTriggerBy != structs.EvalTriggerJobDeregister &&
			alloc.ClientStatus == structs.AllocClientStatusUnknown {
			evalTriggerBy = structs.EvalTriggerReconnect
		}

		// If we weren't able to determine one of our expected eval triggers,
		// continue and don't create an eval.
		if evalTriggerBy == "" {
			continue
		}

		eval = &structs.Evaluation{
			ID:          uuid.Generate(),
			Namespace:   alloc.Namespace,
			TriggeredBy: evalTriggerBy,
			JobID:       alloc.JobID,
			Type:        jobType,
			Priority:    jobPriority,
			Status:      structs.EvalStatusPending,
			CreateTime:  now.UTC().UnixNano(),
			ModifyTime:  now.UTC().UnixNano(),
		}
		evals = append(evals, eval)
	}

	// Add this to the batch
	n.updatesLock.Lock()
	n.updates = append(n.updates, args.Alloc...)
	n.evals = append(n.evals, evals...)

	// Start a new batch if none
	future := n.updateFuture
	if future == nil {
		future = structs.NewBatchFuture()
		n.updateFuture = future
		n.updateTimer = time.AfterFunc(batchUpdateInterval, func() {
			// Get the pending updates
			n.updatesLock.Lock()
			updates := n.updates
			evals := n.evals
			future := n.updateFuture

			// Assume future update patterns will be similar to
			// current batch and set cap appropriately to avoid
			// slice resizing.
			n.updates = make([]*structs.Allocation, 0, len(updates))
			n.evals = make([]*structs.Evaluation, 0, len(evals))

			n.updateFuture = nil
			n.updateTimer = nil
			n.updatesLock.Unlock()

			// Perform the batch update
			n.batchUpdate(future, updates, evals)
		})
	}
	n.updatesLock.Unlock()

	// Wait for the future
	if err := future.Wait(); err != nil {
		return err
	}

	// Setup the response
	reply.Index = future.Index()
	return nil
}

// batchUpdate is used to update all the allocations
func (n *Node) batchUpdate(future *structs.BatchFuture, updates []*structs.Allocation, evals []*structs.Evaluation) {
	var mErr multierror.Error
	// Group pending evals by jobID to prevent creating unnecessary evals
	evalsByJobId := make(map[structs.NamespacedID]struct{})
	var trimmedEvals []*structs.Evaluation
	for _, eval := range evals {
		namespacedID := structs.NamespacedID{
			ID:        eval.JobID,
			Namespace: eval.Namespace,
		}
		_, exists := evalsByJobId[namespacedID]
		if !exists {
			now := time.Now().UTC().UnixNano()
			eval.CreateTime = now
			eval.ModifyTime = now
			trimmedEvals = append(trimmedEvals, eval)
			evalsByJobId[namespacedID] = struct{}{}
		}
	}

	if len(trimmedEvals) > 0 {
		n.logger.Debug("adding evaluations for rescheduling failed allocations", "num_evals", len(trimmedEvals))
	}
	// Prepare the batch update
	batch := &structs.AllocUpdateRequest{
		Alloc:        updates,
		Evals:        trimmedEvals,
		WriteRequest: structs.WriteRequest{Region: n.srv.config.Region},
	}

	// Commit this update via Raft
	_, index, err := n.srv.raftApply(structs.AllocClientUpdateRequestType, batch)
	if err != nil {
		n.logger.Error("alloc update failed", "error", err)
		mErr.Errors = append(mErr.Errors, err)
	}

	// Respond to the future
	future.Respond(index, mErr.ErrorOrNil())
}

// List is used to list the available nodes
func (n *Node) List(args *structs.NodeListRequest,
	reply *structs.NodeListResponse) error {

	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("Node.List", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("node", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "list"}, time.Now())

	// Check node read permissions
	if aclObj, err := n.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.AllowNodeRead() {
		return structs.ErrPermissionDenied
	}

	// Set up the blocking query.
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {

			var err error
			var iter memdb.ResultIterator
			if prefix := args.QueryOptions.Prefix; prefix != "" {
				iter, err = state.NodesByIDPrefix(ws, prefix)
			} else {
				iter, err = state.Nodes(ws)
			}
			if err != nil {
				return err
			}

			pager, err := paginator.NewPaginator(iter, args.QueryOptions, nil,
				paginator.IDTokenizer[*structs.Node](args.NextToken),
				func(node *structs.Node) (*structs.NodeListStub, error) {
					return node.Stub(args.Fields), nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(
					http.StatusBadRequest, "failed to create result paginator: %v", err)
			}

			nodes, nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(
					http.StatusBadRequest, "failed to read result page: %v", err)
			}

			// Populate the reply.
			reply.Nodes = nodes
			reply.NextToken = nextToken

			// Use the last index that affected the jobs table
			index, err := state.Index("nodes")
			if err != nil {
				return err
			}
			reply.Index = index

			// Set the query response
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// createNodeEvals is used to create evaluations for each alloc on a node.
// Each Eval is scoped to a job, so we need to potentially trigger many evals.
func (n *Node) createNodeEvals(node *structs.Node, nodeIndex uint64) ([]string, uint64, error) {
	nodeID := node.ID

	// Snapshot the state
	snap, err := n.srv.fsm.State().Snapshot()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to snapshot state: %v", err)
	}

	// Find all the allocations for this node
	allocs, err := snap.AllocsByNode(nil, nodeID)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to find allocs for '%s': %v", nodeID, err)
	}

	sysJobsIter, err := snap.JobsByScheduler(nil, "system")
	if err != nil {
		return nil, 0, fmt.Errorf("failed to find system jobs for '%s': %v", nodeID, err)
	}

	var sysJobs []*structs.Job
	for jobI := sysJobsIter.Next(); jobI != nil; jobI = sysJobsIter.Next() {
		job := jobI.(*structs.Job)
		// Avoid creating evals for jobs that don't run in this datacenter or
		// node pool. We could perform an entire feasibility check here, but
		// datacenter/pool is a good optimization to start with as their
		// cardinality tends to be low so the check shouldn't add much work.
		if node.IsInPool(job.NodePool) && node.IsInAnyDC(job.Datacenters) {
			sysJobs = append(sysJobs, job)
		}
	}

	// Fast-path if nothing to do
	if len(allocs) == 0 && len(sysJobs) == 0 {
		return nil, 0, nil
	}

	// Create an eval for each JobID affected
	var evals []*structs.Evaluation
	var evalIDs []string
	jobIDs := map[structs.NamespacedID]struct{}{}
	now := time.Now().UTC().UnixNano()

	for _, alloc := range allocs {
		// Deduplicate on JobID
		if _, ok := jobIDs[alloc.JobNamespacedID()]; ok {
			continue
		}
		jobIDs[alloc.JobNamespacedID()] = struct{}{}

		// If it's a sysbatch job, skip it. Sysbatch job evals should only ever
		// be created by periodic-job if they are periodic, and job-register or
		// job-scaling if they are not. Calling the system scheduler by
		// node-update trigger can cause unnecessary or premature allocations
		// to be created.
		if alloc.Job.Type == structs.JobTypeSysBatch {
			continue
		}

		// Create a new eval
		eval := &structs.Evaluation{
			ID:              uuid.Generate(),
			Namespace:       alloc.Namespace,
			Priority:        alloc.Job.Priority,
			Type:            alloc.Job.Type,
			TriggeredBy:     structs.EvalTriggerNodeUpdate,
			JobID:           alloc.JobID,
			NodeID:          nodeID,
			NodeModifyIndex: nodeIndex,
			Status:          structs.EvalStatusPending,
			CreateTime:      now,
			ModifyTime:      now,
		}

		evals = append(evals, eval)
		evalIDs = append(evalIDs, eval.ID)
	}

	// Create an evaluation for each system job.
	for _, job := range sysJobs {
		// Still dedup on JobID as the node may already have the system job.
		if _, ok := jobIDs[job.NamespacedID()]; ok {
			continue
		}
		jobIDs[job.NamespacedID()] = struct{}{}

		// Create a new eval
		eval := &structs.Evaluation{
			ID:              uuid.Generate(),
			Namespace:       job.Namespace,
			Priority:        job.Priority,
			Type:            job.Type,
			TriggeredBy:     structs.EvalTriggerNodeUpdate,
			JobID:           job.ID,
			NodeID:          nodeID,
			NodeModifyIndex: nodeIndex,
			Status:          structs.EvalStatusPending,
			CreateTime:      now,
			ModifyTime:      now,
		}
		evals = append(evals, eval)
		evalIDs = append(evalIDs, eval.ID)
	}

	// Create the Raft transaction
	update := &structs.EvalUpdateRequest{
		Evals:        evals,
		WriteRequest: structs.WriteRequest{Region: n.srv.config.Region},
	}

	// Commit this evaluation via Raft
	// XXX: There is a risk of partial failure where the node update succeeds
	// but that the EvalUpdate does not.
	_, evalIndex, err := n.srv.raftApply(structs.EvalUpdateRequestType, update)
	if err != nil {
		return nil, 0, err
	}
	return evalIDs, evalIndex, nil
}

func (n *Node) EmitEvents(args *structs.EmitNodeEventsRequest, reply *structs.EmitNodeEventsResponse) error {
	aclObj, err := n.srv.AuthenticateClientOnly(n.ctx, args)
	n.srv.MeasureRPCRate("node", structs.RateMetricWrite, args)
	if err != nil {
		return structs.ErrPermissionDenied
	}

	if done, err := n.srv.forward("Node.EmitEvents", args, args, reply); done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "client", "emit_events"}, time.Now())

	if !aclObj.AllowClientOp() {
		return structs.ErrPermissionDenied
	}

	if len(args.NodeEvents) == 0 {
		return fmt.Errorf("no node events given")
	}
	for nodeID, events := range args.NodeEvents {
		if len(events) == 0 {
			return fmt.Errorf("no node events given for node %q", nodeID)
		}
	}

	_, index, err := n.srv.raftApply(structs.UpsertNodeEventsType, args)
	if err != nil {
		n.logger.Error("upserting node events failed", "error", err)
		return err
	}

	reply.Index = index
	return nil
}
