package mqttpubsub

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/brocaar/loraserver/api/gw"
	"github.com/brocaar/lorawan"
	"github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
)

// Backend implements a MQTT pub-sub backend.
type Backend struct {
	conn         mqtt.Client
	txPacketChan chan gw.TXPacketBytes
	gateways     map[lorawan.EUI64]struct{}
	mutex        sync.RWMutex
}

// NewBackend creates a new Backend.
func NewBackend(server, username, password, cafile, certFile, certKeyFile string) (*Backend, error) {
	b := Backend{
		txPacketChan: make(chan gw.TXPacketBytes),
		gateways:     make(map[lorawan.EUI64]struct{}),
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(server)
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetOnConnectHandler(b.onConnected)
	opts.SetConnectionLostHandler(b.onConnectionLost)

	tlsconfig, err := NewTLSConfig(cafile, certFile, certKeyFile)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"ca_cert":  cafile,
			"tls_cert": certFile,
			"tls_key":  certKeyFile,
		}).Fatal("error loading mqtt certificate files")
	}
	if tlsconfig != nil {
		opts.SetTLSConfig(tlsconfig)
	}

	log.WithField("server", server).Info("backend: connecting to mqtt broker")
	b.conn = mqtt.NewClient(opts)
	if token := b.conn.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	return &b, nil
}

// NewTLSConfig returns the TLS configuration.
func NewTLSConfig(cafile, certFile, certKeyFile string) (*tls.Config, error) {
	// Here are three valid options:
	//   - Only CA
	//   - TLS cert + key
	//   - CA, TLS cert + key

	if cafile == "" && certFile == "" && certKeyFile == "" {
		log.Info("backend: TLS config is empty")
		return nil, nil
	}

	tlsConfig := &tls.Config{}

	// Import trusted certificates from CAfile.pem.
	if cafile != "" {
		cacert, err := ioutil.ReadFile(cafile)
		if err != nil {
			log.Errorf("backend: couldn't load cafile: %s", err)
			return nil, err
		}
		certpool := x509.NewCertPool()
		certpool.AppendCertsFromPEM(cacert)

		tlsConfig.RootCAs = certpool // RootCAs = certs used to verify server cert.
	}

	// Import certificate and the key
	if certFile != "" && certKeyFile != "" {
		kp, err := tls.LoadX509KeyPair(certFile, certKeyFile)
		if err != nil {
			log.Errorf("backend: couldn't load MQTT TLS key pair: %s", err)
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{kp}
	}

	return tlsConfig, nil
}

// Close closes the backend.
func (b *Backend) Close() {
	b.conn.Disconnect(250) // wait 250 milisec to complete pending actions
}

// TXPacketChan returns the TXPacketBytes channel.
func (b *Backend) TXPacketChan() chan gw.TXPacketBytes {
	return b.txPacketChan
}

// SubscribeGatewayTX subscribes the backend to the gateway TXPacketBytes
// topic (packets the gateway needs to transmit).
func (b *Backend) SubscribeGatewayTX(mac lorawan.EUI64) error {
	defer b.mutex.Unlock()
	b.mutex.Lock()

	topic := fmt.Sprintf("gateway/%s/tx", mac.String())
	log.WithField("topic", topic).Info("backend: subscribing to topic")
	if token := b.conn.Subscribe(topic, 0, b.txPacketHandler); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	b.gateways[mac] = struct{}{}
	return nil
}

// UnSubscribeGatewayTX unsubscribes the backend from the gateway TXPacketBytes
// topic.
func (b *Backend) UnSubscribeGatewayTX(mac lorawan.EUI64) error {
	defer b.mutex.Unlock()
	b.mutex.Lock()

	topic := fmt.Sprintf("gateway/%s/tx", mac.String())
	log.WithField("topic", topic).Info("backend: unsubscribing from topic")
	if token := b.conn.Unsubscribe(topic); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	delete(b.gateways, mac)
	return nil
}

// PublishGatewayRX publishes a RX packet to the MQTT broker.
func (b *Backend) PublishGatewayRX(mac lorawan.EUI64, rxPacket gw.RXPacketBytes) error {
	topic := fmt.Sprintf("gateway/%s/rx", mac.String())
	return b.publish(topic, rxPacket)
}

// PublishGatewayStats publishes a GatewayStatsPacket to the MQTT broker.
func (b *Backend) PublishGatewayStats(mac lorawan.EUI64, stats gw.GatewayStatsPacket) error {
	topic := fmt.Sprintf("gateway/%s/stats", mac.String())
	return b.publish(topic, stats)
}

// PublishGatewayTXAck publishes a TX ack to the MQTT broker.
func (b *Backend) PublishGatewayTXAck(mac lorawan.EUI64, ack gw.TXAck) error {
	topic := fmt.Sprintf("gateway/%s/ack", mac.String())
	return b.publish(topic, ack)
}

func (b *Backend) publish(topic string, v interface{}) error {
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	log.WithField("topic", topic).Info("backend: publishing packet")
	if token := b.conn.Publish(topic, 0, false, bytes); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	return nil
}

func (b *Backend) txPacketHandler(c mqtt.Client, msg mqtt.Message) {
	log.WithField("topic", msg.Topic()).Info("backend: packet received")
	var txPacket gw.TXPacketBytes
	if err := json.Unmarshal(msg.Payload(), &txPacket); err != nil {
		log.Errorf("backend: decode tx packet error: %s", err)
		return
	}
	b.txPacketChan <- txPacket
}

func (b *Backend) onConnected(c mqtt.Client) {
	defer b.mutex.RUnlock()
	b.mutex.RLock()

	log.Info("backend: connected to mqtt broker")
	if len(b.gateways) > 0 {
		for {
			log.WithField("topic_count", len(b.gateways)).Info("backend: re-registering to gateway topics")
			topics := make(map[string]byte)
			for k := range b.gateways {
				topics[fmt.Sprintf("gateway/%s/tx", k)] = 0
			}
			if token := b.conn.SubscribeMultiple(topics, b.txPacketHandler); token.Wait() && token.Error() != nil {
				log.WithField("topic_count", len(topics)).Errorf("backend: subscribe multiple failed: %s", token.Error())
				time.Sleep(time.Second)
				continue
			}
			return
		}
	}
}

func (b *Backend) onConnectionLost(c mqtt.Client, reason error) {
	log.Errorf("backend: mqtt connection error: %s", reason)
}
