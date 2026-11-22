package bridge

import (
	"context"
	"fmt"
	"strings"
)

// command is one parsed HA command, already resolved to a service name.
type command struct {
	target string // serialization key, e.g. "svc:media-plex"
	label  string // for the Last command sensor, e.g. "stop media-plex"
	run    func(ctx context.Context) error
}

// handle routes one inbound MQTT message. It runs on the client's receive
// path, so it never blocks.
func (b *Bridge) handle(topic string, payload []byte, retained bool) {
	switch topic {
	case b.birthTopic():
		if string(payload) == "online" {
			b.send(event{kind: evBirth})
		}
		return
	case b.discoveryTopic():
		if retained {
			b.send(event{kind: evDiscovery, payload: payload})
		}
		return
	}
	if retained {
		// A retained command would replay on every reconnect.
		log.Warn("ignoring retained command", "topic", topic)
		return
	}
	cmd, ok := b.parse(topic, string(payload))
	if !ok {
		log.Warn("ignoring unknown command", "topic", topic, "payload", string(payload))
		return
	}
	go b.execute(cmd)
}

// parse maps a command topic to an API call. Slugs resolve through the
// discovery index, so raw topic text never reaches the API.
func (b *Bridge) parse(topic, payload string) (command, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "homelab" || parts[1] != "svc" {
		return command{}, false
	}
	name, ok := b.currentIndex().services[parts[2]]
	if !ok {
		return command{}, false
	}
	api := b.api
	c := command{target: "svc:" + name}
	switch {
	case parts[3] == "set" && payload == "ON":
		c.label, c.run = "start "+name, func(ctx context.Context) error { return api.Start(ctx, name) }
	case parts[3] == "set" && payload == "OFF":
		c.label, c.run = "stop "+name, func(ctx context.Context) error { return api.Stop(ctx, name) }
	case parts[3] == "restart":
		c.label, c.run = "restart "+name, func(ctx context.Context) error { return api.Restart(ctx, name) }
	default:
		return command{}, false
	}
	return c, true
}

// execute runs one command, one at a time per target, and reports the
// outcome to the publisher.
func (b *Bridge) execute(cmd command) {
	b.mu.Lock()
	slot, ok := b.targets[cmd.target]
	if !ok {
		slot = make(chan struct{}, 1)
		b.targets[cmd.target] = slot
	}
	b.mu.Unlock()

	select {
	case slot <- struct{}{}:
	case <-b.runCtx.Done():
		return
	}
	defer func() { <-slot }()
	ctx, cancel := context.WithTimeout(b.runCtx, commandTimeout)
	defer cancel()
	if err := cmd.run(ctx); err != nil {
		log.Warn("home assistant command failed", "command", cmd.label, "error", err)
		b.finish(fmt.Sprintf("%s: %v", cmd.label, err))
		return
	}
	log.Info("home assistant command", "command", cmd.label)
	b.finish(cmd.label + ": ok")
}

func (b *Bridge) finish(text string) {
	select {
	case b.events <- event{kind: evResult, text: text}:
	case <-b.runCtx.Done():
	}
}
