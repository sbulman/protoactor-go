// Copyright (C) 2015-2022 Asynkron AB All rights reserved

package cluster

import (
	"errors"
	"fmt"
	"time"

	"github.com/asynkron/gofun/set"
	"google.golang.org/protobuf/proto"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/asynkron/protoactor-go/log"
	"google.golang.org/protobuf/types/known/anypb"
)

const DefaultGossipActorName string = "gossip"

// Used to update gossip data when a Clustertopology event occurs
type GossipUpdate struct {
	MemberID, Key string
	Value         *anypb.Any
	SeqNumber     int64
}

// Customary type used to provide consensus check callbacks of any type
// note: this is equivalent to (for future go v1.18):
//	type ConsensusChecker[T] func(GossipState, map[string]empty) (bool, T)
type ConsensusChecker func(*GossipState, map[string]empty) (bool, interface{})

// The Gossiper data structure manages Gossip
type Gossiper struct {
	// The Gossiper Actor Name, defaults to "gossip"
	GossipActorName string

	// The Gossiper Cluster
	cluster *Cluster

	// The actor PID
	pid *actor.PID

	// Channel use to stop the gossip loop
	close chan struct{}

	// Message throttler
	throttler actor.ShouldThrottle
}

// Creates a new Gossiper value and return it back
func newGossiper(cl *Cluster, opts ...Option) (Gossiper, error) {
	// create a new Gossiper value
	gossiper := Gossiper{
		GossipActorName: DefaultGossipActorName,
		cluster:         cl,
		close:           make(chan struct{}),
	}

	// apply any given options
	for _, opt := range opts {
		opt(&gossiper)
	}

	return gossiper, nil
}

func (g *Gossiper) GetState(key string) (map[string]*anypb.Any, error) {
	plog.Debug(fmt.Sprintf("Gossiper getting state from %s", g.pid))

	msg := NewGetGossipStateRequest(key)
	timeout := g.cluster.Config.TimeoutTime
	r, err := g.cluster.ActorSystem.Root.RequestFuture(g.pid, &msg, timeout).Result()
	if err != nil {
		switch err {
		case actor.ErrTimeout:
			plog.Error("Could not get a response from GossipActor: request timeout", log.Error(err), log.String("remote", g.pid.String()))
			return nil, err
		case actor.ErrDeadLetter:
			plog.Error("remote no longer exists", log.Error(err), log.String("remote", g.pid.String()))
			return nil, err
		default:
			plog.Error("Could not get a response from GossipActor", log.Error(err), log.String("remote", g.pid.String()))
			return nil, err
		}
	}

	// try to cast the response to GetGossipStateResponse concrete value
	response, ok := r.(*GetGossipStateResponse)
	if !ok {
		err := fmt.Errorf("could not promote %T interface to GetGossipStateResponse", r)
		plog.Error("Could not get a response from GossipActor", log.Error(err), log.String("remote", g.pid.String()))
		return nil, err
	}

	return response.State, nil
}

// Sends fire and forget message to update member state
func (g *Gossiper) SetState(key string, value proto.Message) {
	if g.throttler() == actor.Open {
		plog.Debug(fmt.Sprintf("Gossiper setting state %s to %s", key, g.pid))
	}

	if g.pid == nil {
		return
	}

	msg := NewGossipStateKey(key, value)
	g.cluster.ActorSystem.Root.Send(g.pid, &msg)
}

// Sends a Request (that blocks) to update member state
func (g *Gossiper) SetStateRequest(key string, value proto.Message) error {
	if g.throttler() == actor.Open {
		plog.Debug(fmt.Sprintf("Gossiper setting state %s to %s", key, g.pid))
	}

	if g.pid == nil {
		return errors.New("gossiper Actor PID is nil")
	}

	msg := NewGossipStateKey(key, value)
	r, err := g.cluster.ActorSystem.Root.RequestFuture(g.pid, &msg, g.cluster.Config.TimeoutTime).Result()
	if err != nil {
		if err == actor.ErrTimeout {
			plog.Error("Could not get a response from Gossiper Actor: request timeout", log.String("remote", g.pid.String()))
			return err
		}
		plog.Error("Could not get a response from Gossiper Actor", log.Error(err), log.String("remote", g.pid.String()))
		return err
	}

	// try to cast the response to SetGossipStateResponse concrete value
	_, ok := r.(*SetGossipStateResponse)
	if !ok {
		err := fmt.Errorf("could not promote %T interface to SetGossipStateResponse", r)
		plog.Error("Could not get a response from Gossip Actor", log.Error(err), log.String("remote", g.pid.String()))
		return err
	}
	return nil
}

func (g *Gossiper) SendState() {
	if g.pid == nil {
		return
	}

	_, err := g.cluster.ActorSystem.Root.RequestFuture(g.pid, &SendGossipStateRequest{}, 5*time.Second).Result()
	if err != nil {
		plog.Warn("Gossip could not send gossip request", log.PID("PID", g.pid), log.Error(err))
		return
	}

	// It appears that both GossipResponse and GossipResponseAck can be the result
	// of the above request. Not clear why at the moment, but this error is filling
	// up logs. The system 'appears' to work. Need to investigate further.

	// if _, ok := r.(*SendGossipStateResponse); !ok {
	// 	plog.Error("Gossip SendState received unknown response", log.Message(r))
	// }
}

// Builds a consensus handler and a consensus checker, send the checker to the
// Gossip actor and returns the handler back to the caller
func (g *Gossiper) RegisterConsensusCheck(key string, getValue func(*anypb.Any) interface{}) ConsensusHandler {
	definition := NewConsensusCheckBuilder(key, getValue)
	consensusHandle, check := definition.Build()
	request := NewAddConsensusCheck(consensusHandle.GetID(), check)
	g.cluster.ActorSystem.Root.Send(g.pid, &request)
	return consensusHandle
}

func (g *Gossiper) StartGossiping() error {
	var err error
	g.pid, err = g.cluster.ActorSystem.Root.SpawnNamed(actor.PropsFromProducer(func() actor.Actor {
		return NewGossipActor(
			g.cluster.Config.GossipRequestTimeout,
			g.cluster.ActorSystem.ID,
			func() set.Set[string] {
				return g.cluster.GetBlockedMembers()
			},
			g.cluster.Config.GossipFanOut,
			g.cluster.Config.GossipMaxSend,
		)
	}), g.GossipActorName)

	if err != nil {
		plog.Error("Failed to start gossip actor", log.Error(err))
		return err
	}

	g.cluster.ActorSystem.EventStream.Subscribe(func(evt interface{}) {
		if topology, ok := evt.(*ClusterTopology); ok {
			g.cluster.ActorSystem.Root.Send(g.pid, topology)
		}
	})
	plog.Info("Started Cluster Gossip")
	g.throttler = actor.NewThrottle(3, 60*time.Second, g.throttledLog)
	go g.gossipLoop()

	return nil
}

func (g *Gossiper) Shutdown() {
	if g.pid == nil {
		return
	}

	plog.Info("Shutting down gossip")
	g.cluster.ActorSystem.Root.Stop(g.pid)
	plog.Info("Shut down gossip")
}

func (g *Gossiper) gossipLoop() {
	plog.Info("Starting gossip loop")

	// create a ticker that will tick each GossipInterval milliseconds
	// we do not use sleep as sleep puts the goroutine out of the scheduler
	// P and we do not want our Gs to be scheduled out from the running Ms
	ticker := time.NewTicker(g.cluster.Config.GossipInterval)
breakLoop:
	for {
		select {
		case <-g.close:
			plog.Info("Stopping Gossip Loop")
			break breakLoop
		case <-ticker.C:
			g.SetState(HearthbeatKey, &MemberHeartbeat{
				// todo collect the actor statistics
				ActorStatistics: &ActorStatistics{},
			})
			g.SendState()

			/*
			 await BlockExpiredHeartbeats();

			 await BlockGracefullyLeft();
			*/
		}
	}
}

func (g *Gossiper) throttledLog(counter int32) {
	plog.Debug(fmt.Sprintf("[Gossiper] Gossiper Setting State to %s", g.pid), log.Int("throttled", int(counter)))
}
