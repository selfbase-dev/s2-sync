package service

import "time"

type EventType string

const (
	EventStarted EventType = "started"
	EventStopped EventType = "stopped"
	EventSynced  EventType = "synced"
	EventError   EventType = "error"
	EventLog     EventType = "log"
)

type Event struct {
	Type    EventType `json:"type"`
	Message string    `json:"message,omitempty"`
	Mount   *Mount    `json:"mount,omitempty"`
	Time    time.Time `json:"time"`
}
