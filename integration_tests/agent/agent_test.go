package agent_test

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/loggregator/plumbing"
	v2 "code.cloudfoundry.org/loggregator/plumbing/v2"
	"code.cloudfoundry.org/loggregator/testservers"
	"github.com/cloudfoundry/dropsonde/emitter"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var _ = Describe("Agent", func() {
	It("accepts connections on the v1 API", func() {
		consumerServer, err := NewServer()
		Expect(err).ToNot(HaveOccurred())
		defer consumerServer.Stop()
		agentCleanup, agentPorts := testservers.StartAgent(
			testservers.BuildAgentConfig("127.0.0.1", consumerServer.Port()),
		)
		defer agentCleanup()

		udpEmitter, err := emitter.NewUdpEmitter(fmt.Sprintf("127.0.0.1:%d", agentPorts.UDP))
		Expect(err).ToNot(HaveOccurred())
		eventEmitter := emitter.NewEventEmitter(udpEmitter, "some-origin")

		go func() {
			event := &events.CounterEvent{
				Name:  proto.String("some-name"),
				Delta: proto.Uint64(5),
				Total: proto.Uint64(6),
			}

			for {
				eventEmitter.Emit(event)
			}
		}()

		var rx plumbing.DopplerIngestor_PusherServer
		Eventually(consumerServer.V1.PusherInput.Arg0).Should(Receive(&rx))

		e := make(chan *plumbing.EnvelopeData)
		go func() {
			for {
				data, err := rx.Recv()
				if err != nil {
					return
				}
				e <- data
			}
		}()

		var data *plumbing.EnvelopeData
		Eventually(e).Should(Receive(&data))

		envelope := new(events.Envelope)
		Expect(envelope.Unmarshal(data.Payload)).To(Succeed())
	})

	It("accepts connections on the v2 API", func() {
		consumerServer, err := NewServer()
		Expect(err).ToNot(HaveOccurred())
		defer consumerServer.Stop()
		agentCleanup, agentPorts := testservers.StartAgent(
			testservers.BuildAgentConfig("127.0.0.1", consumerServer.Port()),
		)
		defer agentCleanup()

		emitEnvelope := &v2.Envelope{
			Message: &v2.Envelope_Log{
				Log: &v2.Log{
					Payload: []byte("some-message"),
					Type:    v2.Log_OUT,
				},
			},
		}

		client := agentClient(agentPorts.GRPC)
		sender, err := client.Sender(context.Background())
		Expect(err).ToNot(HaveOccurred())

		go func() {
			for range time.Tick(time.Nanosecond) {
				sender.Send(emitEnvelope)
			}
		}()

		var rx v2.Ingress_BatchSenderServer
		numDopplerConnections := 5
		for i := 0; i < numDopplerConnections; i++ {
			Eventually(consumerServer.V2.BatchSenderInput.Arg0).Should(Receive(&rx))
			consumerServer.V2.BatchSenderOutput.Ret0 <- nil
		}
		Eventually(consumerServer.V2.BatchSenderInput.Arg0).Should(Receive(&rx))

		var envBatch *v2.EnvelopeBatch
		var idx int
		f := func() *v2.Envelope {
			batch, err := rx.Recv()
			Expect(err).ToNot(HaveOccurred())

			defer func() { envBatch = batch }()

			for i, envelope := range batch.Batch {
				if envelope.GetLog() != nil {
					idx = i
					return envelope
				}
			}

			return nil
		}
		Eventually(f, 10).ShouldNot(BeNil())

		Expect(len(envBatch.Batch)).ToNot(BeZero())

		Expect(*envBatch.Batch[idx]).To(MatchFields(IgnoreExtras, Fields{
			"Message": Equal(&v2.Envelope_Log{
				Log: &v2.Log{Payload: []byte("some-message")},
			}),
			"DeprecatedTags": Equal(map[string]*v2.Value{
				"auto-tag-1": {
					Data: &v2.Value_Text{"auto-tag-value-1"},
				},
				"auto-tag-2": {
					Data: &v2.Value_Text{"auto-tag-value-2"},
				},
			}),
		}))
	})
})

func HomeAddrToPort(addr net.Addr) int {
	port, err := strconv.Atoi(strings.Replace(addr.String(), "127.0.0.1:", "", 1))
	if err != nil {
		panic(err)
	}
	return port
}

func agentClient(port int) v2.IngressClient {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	tlsConfig, err := plumbing.NewClientMutualTLSConfig(
		testservers.Cert("metron.crt"),
		testservers.Cert("metron.key"),
		testservers.Cert("loggregator-ca.crt"),
		"metron",
	)
	if err != nil {
		panic(err)
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		panic(err)
	}
	return v2.NewIngressClient(conn)
}
