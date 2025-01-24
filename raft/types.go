package raft

import (
	"github.com/anthdm/hollywood/actor"
)

type MessageHandler func(mg *Message)

type nodeMetadata struct {
	pid        *actor.PID
	nextIndex  uint64
	matchIndex uint64
}

type commandMetadata struct {
	sender *actor.PID
}
