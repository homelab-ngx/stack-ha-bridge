package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// reasonDisconnectWithWill asks the broker to publish the will (offline) on
// a clean shutdown, which a normal DISCONNECT suppresses.
const reasonDisconnectWithWill = 0x04

type pahoPublisher struct{ cm *autopaho.ConnectionManager }

func (p pahoPublisher) publish(ctx context.Context, topic string, payload []byte, retain bool) error {
	qos := byte(0)
	if retain {
		qos = 1
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := p.cm.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, Retain: retain, QoS: qos})
	return err
}

// Run connects to the broker and serves until ctx ends. Only a broker URL
// that cannot be parsed is an error; everything else (a missing password,
// an unreachable broker or daemon) is logged and retried, so the unit never
// crash-loops.
func (b *Bridge) Run(ctx context.Context) error {
	b.runCtx = ctx
	broker, err := url.Parse(b.cfg.BrokerURL)
	if err != nil || broker.Host == "" {
		return fmt.Errorf("bad broker URL %q", b.cfg.BrokerURL)
	}
	if readPassword(b.cfg.PasswordFile) == "" {
		log.Warn("no broker password yet; connecting will fail until it is there", "file", b.cfg.PasswordFile)
	}

	// The discovery topic is subscribed without NoLocal so the broker hands
	// back the payload retained on the previous run (by this bridge, or by
	// the daemon before the bridge moved into a stack).
	subs := []paho.SubscribeOptions{
		{Topic: b.birthTopic(), QoS: 1},
		{Topic: b.discoveryTopic(), QoS: 1},
	}
	for _, f := range commandFilters() {
		subs = append(subs, paho.SubscribeOptions{Topic: f, QoS: 1, NoLocal: true})
	}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{broker},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		ReconnectBackoff: func(n int) time.Duration {
			return min(time.Duration(n+1)*10*time.Second, 2*time.Minute)
		},
		WillMessage: &paho.WillMessage{Topic: topicStatus, Payload: []byte("offline"), QoS: 1, Retain: true},
		ConnectPacketBuilder: func(c *paho.Connect, _ *url.URL) (*paho.Connect, error) {
			c.Username, c.UsernameFlag = b.cfg.Username, true
			c.Password, c.PasswordFlag = []byte(readPassword(b.cfg.PasswordFile)), true
			return c, nil
		},
		DisconnectPacketBuilder: func() *paho.Disconnect {
			return &paho.Disconnect{ReasonCode: reasonDisconnectWithWill}
		},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Info("connected to the Home Assistant broker", "broker", broker.Host)
			b.connected.Store(true)
			go func() {
				sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if _, err := cm.Subscribe(sctx, &paho.Subscribe{Subscriptions: subs}); err != nil {
					// Publishing needs no subscription; commands and the
					// readback wait for the next connection.
					log.Warn("subscribe failed", "error", err)
				}
				b.send(event{kind: evConnected})
			}()
		},
		OnConnectionDown: func() bool {
			b.connected.Store(false)
			log.Warn("lost the Home Assistant broker connection")
			return true
		},
		OnConnectError: func(err error) {
			log.Warn("cannot connect to the Home Assistant broker", "broker", broker.Host, "error", err)
		},
		ClientConfig: paho.ClientConfig{
			ClientID: b.cfg.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					b.handle(pr.Packet.Topic, pr.Packet.Payload, pr.Packet.Retain)
					return true, nil
				},
			},
		},
	}

	// The connection outlives ctx just long enough to disconnect with the will.
	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	cm, err := autopaho.NewConnection(connCtx, cfg)
	if err != nil {
		return fmt.Errorf("mqtt: %w", err)
	}
	b.pub = pahoPublisher{cm}
	b.loop(ctx)

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	if err := cm.Disconnect(dctx); err != nil && !errors.Is(err, autopaho.ConnectionDownError) {
		log.Warn("disconnect from the Home Assistant broker", "error", err)
	}
	return nil
}
