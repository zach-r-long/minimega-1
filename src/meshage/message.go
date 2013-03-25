package meshage

import (
	"fmt"
	log "minilog"
)

const (
	ACK = iota
	MSA
	MESSAGE
)

const (
	LOLLIPOP_LENGTH = 16
)

// A message is the payload for all message passing, and contains the user
// specified message in the body field.
type Message struct {
	Recipients   []string    // list of client recipients, unused if broadcasting
	Source       string      // source node name
	CurrentRoute []string    // list of hops for an in-flight message
	ID           uint64      // sequence ID, uses lollipop sequence numbering
	Command      int         // mesh state announcement, message
	Body         interface{} // message body
}

// Send a message according to the parameters set in the message.
// Users will generally use the Set and Broadcast functions instead of Send.
// The returned error is always nil if the message type is broadcast.
// If an error is encountered, Send returns immediately.
func (n *Node) Send(m *Message) error {
	log.Debug("Send: %v\n", m)
	routeSlices := make(map[string][]string)
	for _, v := range m.Recipients {
		if v == n.name {
			continue
		}

		var route string
		var ok bool
		if route, ok = n.routes[v]; !ok {
			n.updateRoute(v)
			if route, ok = n.routes[v]; !ok {
				return fmt.Errorf("no route to host: %v", v)
			}
		}
		routeSlices[route] = append(routeSlices[route], v)
	}

	log.Debug("routeSlices: %v\n", routeSlices)


	errChan := make(chan error)
	for k, v := range routeSlices {
		go func(client string, recipients []string) {
			mOne := &Message{
				Recipients:   recipients,
				Source:       m.Source,
				CurrentRoute: m.CurrentRoute,
				Command:      m.Command,
				Body:         m.Body,
			}
			err := n.clientSend(client, mOne)
			if err != nil {
				errChan <- err
			}
			errChan <- nil
		}(k, v)
	}

	// wait on all of the client sends to complete
	var ret string
	for i := 0; i < len(routeSlices); i++ {
		r := <-errChan
		if r != nil {
			ret += r.Error() + "\n"
		}
	}
	if ret == "" {
		return nil
	}

	return fmt.Errorf("%v", ret)
}

// Set sends a message to a set of nodes. Set blocks until an ACK is received
// from all recipient nodes, or until the timeout is reached.
func (n *Node) Set(recipients []string, body interface{}) error {
	m := &Message{
		Recipients:   recipients,
		Source:       n.name,
		CurrentRoute: []string{n.name},
		Command:      MESSAGE,
		Body:         body,
	}
	return n.Send(m)
}

// Broadcast sends a message to all nodes on the mesh.
func (n *Node) Broadcast(body interface{}) (int, error) {
	var recipients []string
	for k, _ := range n.effectiveNetwork {
		if k != n.name {
			recipients = append(recipients, k)
		}
	}
	m := &Message{
		Recipients:   recipients,
		Source:       n.name,
		CurrentRoute: []string{n.name},
		Command:      MESSAGE,
		Body:         body,
	}
	return len(recipients), n.Send(m)
}

// messageHandler accepts messages from all connected clients and forwards them to the
// appropriate handlers, and to the receiver channel should the message be intended for this
// node.
func (n *Node) messageHandler() {
	log.Debugln("messageHandler")
	for {
		m := <-n.messagePump
		log.Debug("messageHandler: %#v\n", m)
		m.CurrentRoute = append(m.CurrentRoute, n.name)

		switch m.Command {
		case MSA:
			n.sequenceLock.Lock()
			if m.ID == 1 && n.sequences[m.Source] > LOLLIPOP_LENGTH {
				n.sequences[m.Source] = 0
			}
			if m.ID > n.sequences[m.Source] {
				n.sequences[m.Source] = m.ID

				go n.handleMSA(m)
				go n.flood(m)
			} else {
				log.Debug("dropping broadcast: %v:%v\n", m.Source, m.ID)
			}
			n.sequenceLock.Unlock()
		case MESSAGE:
			var newRecipients []string
			for _, i := range m.Recipients {
				if i == n.name {
					go n.handleMessage(m)
				} else {
					newRecipients = append(newRecipients, i)
				}
			}
			m.Recipients = newRecipients
			go n.Send(m)
		default:
			n.errors <- fmt.Errorf("invalid message command: %v", m.Command)
		}
	}
}

func (n *Node) flood(m *Message) {
	log.Debug("flood: %v\n", m)
floodLoop:
	for k, _ := range n.clients {
		for _, j := range m.CurrentRoute {
			if k == j {
				continue floodLoop
			}
		}
		go n.clientSend(k, m)
	}
}

func (n *Node) handleMessage(m *Message) {
	n.receive <- m
}