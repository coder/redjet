package redjet

import (
	"fmt"
)

type SubMessageType string

const (
	SubMessageSubscribe SubMessageType = "subscribe"
	SubMessageMessage   SubMessageType = "message"
)

// SubMessage is a message received from a pubsub subscription.
type SubMessage struct {
	// Type is either "subscribe" (acknowledgement of subscription) or "message"
	Type    SubMessageType
	Channel string

	// Payload is the number of channels subscribed to if Type is "subscribe",
	Payload string
}

// NextSubMessage reads the next subscribe from the result.
// Read more: https://redis.io/docs/manual/pubsub/.
//
// It does not close the Result even if CloseOnRead is true.
func (r *Result) NextSubMessage() (*SubMessage, error) {
	// NextSubMessage is implemented without using internal methods to
	// demonstrate how to use the public API.

	ln, err := r.ArrayLength()
	if err != nil {
		return nil, err
	}

	if ln != 3 {
		return nil, fmt.Errorf("expected 3 elements, got %d", ln)
	}

	var msg SubMessage
	for i := 0; i < ln; i++ {
		s, err := r.String()
		if err != nil {
			return nil, err
		}
		switch i {
		case 0:
			msg.Type = SubMessageType(s)
		case 1:
			msg.Channel = s
		case 2:
			msg.Payload = s
		}
	}
	return &msg, nil
}

func isSubscribeCmd(cmd string) bool {
	switch cmd {
	case "SUBSCRIBE", "PSUBSCRIBE", "UNSUBSCRIBE", "PUNSUBSCRIBE", "PING", "QUIT", "RESET":
		return true
	default:
		return false
	}
}
