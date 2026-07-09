package agent

import "errors"

var (
	ErrSessionNotFound = errors.New("agent: session not found")
	ErrSessionFinished = errors.New("agent: session already finished")
	ErrQueueFull       = errors.New("agent: command queue full")
)
