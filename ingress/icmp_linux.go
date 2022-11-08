//go:build linux

package ingress

// This file implements ICMPProxy for Linux. Each (source IP, destination IP, echo ID) opens a non-privileged ICMP socket.
// The source IP of the requests are rewritten to the bind IP of the socket and echo ID rewritten to the port number of
// the socket. The kernel ensures the socket only reads replies whose echo ID matches the port number.
// For more information about the socket, see https://man7.org/linux/man-pages/man7/icmp.7.html and https://lwn.net/Articles/422330/

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"

	"github.com/cloudflare/cloudflared/packet"
	"github.com/cloudflare/cloudflared/tracing"
)

const (
	// https://lwn.net/Articles/550551/ IPv4 and IPv6 share the same path
	pingGroupPath = "/proc/sys/net/ipv4/ping_group_range"
)

var (
	findGroupIDRegex = regexp.MustCompile(`\d+`)
)

type icmpProxy struct {
	srcFunnelTracker *packet.FunnelTracker
	listenIP         netip.Addr
	ipv6Zone         string
	logger           *zerolog.Logger
	idleTimeout      time.Duration
}

func newICMPProxy(listenIP netip.Addr, zone string, logger *zerolog.Logger, idleTimeout time.Duration) (*icmpProxy, error) {
	if err := testPermission(listenIP, zone, logger); err != nil {
		return nil, err
	}
	return &icmpProxy{
		srcFunnelTracker: packet.NewFunnelTracker(),
		listenIP:         listenIP,
		ipv6Zone:         zone,
		logger:           logger,
		idleTimeout:      idleTimeout,
	}, nil
}

func testPermission(listenIP netip.Addr, zone string, logger *zerolog.Logger) error {
	// Opens a non-privileged ICMP socket. On Linux the group ID of the process needs to be in ping_group_range
	// Only check ping_group_range once for IPv4
	if listenIP.Is4() {
		if err := checkInPingGroup(); err != nil {
			logger.Warn().Err(err).Msgf("The user running cloudflared process has a GID (group ID) that is not within ping_group_range. You might need to add that user to a group within that range, or instead update the range to encompass a group the user is already in by modifying %s. Otherwise cloudflared will not be able to ping this network", pingGroupPath)
			return err
		}
	}
	conn, err := newICMPConn(listenIP, zone)
	if err != nil {
		return err
	}
	// This conn is only to test if cloudflared has permission to open this type of socket
	conn.Close()
	return nil
}

func checkInPingGroup() error {
	file, err := os.ReadFile(pingGroupPath)
	if err != nil {
		return err
	}
	groupID := os.Getgid()
	// Example content: 999	   59999
	found := findGroupIDRegex.FindAll(file, 2)
	if len(found) == 2 {
		groupMin, err := strconv.ParseInt(string(found[0]), 10, 32)
		if err != nil {
			return errors.Wrapf(err, "failed to determine minimum ping group ID")
		}
		groupMax, err := strconv.ParseInt(string(found[1]), 10, 32)
		if err != nil {
			return errors.Wrapf(err, "failed to determine minimum ping group ID")
		}
		if groupID < int(groupMin) || groupID > int(groupMax) {
			return fmt.Errorf("Group ID %d is not between ping group %d to %d", groupID, groupMin, groupMax)
		}
		return nil
	}
	return fmt.Errorf("did not find group range in %s", pingGroupPath)
}

func (ip *icmpProxy) Request(ctx context.Context, pk *packet.ICMP, responder *packetResponder) error {
	ctx, span := responder.requestSpan(ctx, pk)
	defer responder.exportSpan()

	originalEcho, err := getICMPEcho(pk.Message)
	if err != nil {
		tracing.EndWithErrorStatus(span, err)
		return err
	}
	span.SetAttributes(
		attribute.Int("originalEchoID", originalEcho.ID),
		attribute.Int("seq", originalEcho.Seq),
	)

	shouldReplaceFunnelFunc := createShouldReplaceFunnelFunc(ip.logger, responder.datagramMuxer, pk, originalEcho.ID)
	newFunnelFunc := func() (packet.Funnel, error) {
		conn, err := newICMPConn(ip.listenIP, ip.ipv6Zone)
		if err != nil {
			tracing.EndWithErrorStatus(span, err)
			return nil, errors.Wrap(err, "failed to open ICMP socket")
		}
		ip.logger.Debug().Msgf("Opened ICMP socket listen on %s", conn.LocalAddr())
		closeCallback := func() error {
			return conn.Close()
		}
		localUDPAddr, ok := conn.LocalAddr().(*net.UDPAddr)
		if !ok {
			return nil, fmt.Errorf("ICMP listener address %s is not net.UDPAddr", conn.LocalAddr())
		}
		span.SetAttributes(attribute.Int("port", localUDPAddr.Port))

		echoID := localUDPAddr.Port
		icmpFlow := newICMPEchoFlow(pk.Src, closeCallback, conn, responder, echoID, originalEcho.ID, packet.NewEncoder())
		return icmpFlow, nil
	}
	funnelID := flow3Tuple{
		srcIP:          pk.Src,
		dstIP:          pk.Dst,
		originalEchoID: originalEcho.ID,
	}
	funnel, isNew, err := ip.srcFunnelTracker.GetOrRegister(funnelID, shouldReplaceFunnelFunc, newFunnelFunc)
	if err != nil {
		tracing.EndWithErrorStatus(span, err)
		return err
	}
	icmpFlow, err := toICMPEchoFlow(funnel)
	if err != nil {
		tracing.EndWithErrorStatus(span, err)
		return err
	}
	if isNew {
		span.SetAttributes(attribute.Bool("newFlow", true))
		ip.logger.Debug().
			Str("src", pk.Src.String()).
			Str("dst", pk.Dst.String()).
			Int("originalEchoID", originalEcho.ID).
			Msg("New flow")
		go func() {
			defer ip.srcFunnelTracker.Unregister(funnelID, icmpFlow)
			if err := ip.listenResponse(ctx, icmpFlow); err != nil {
				ip.logger.Debug().Err(err).
					Str("src", pk.Src.String()).
					Str("dst", pk.Dst.String()).
					Int("originalEchoID", originalEcho.ID).
					Msg("Failed to listen for ICMP echo response")
			}
		}()
	}
	if err := icmpFlow.sendToDst(pk.Dst, pk.Message); err != nil {
		tracing.EndWithErrorStatus(span, err)
		return errors.Wrap(err, "failed to send ICMP echo request")
	}
	tracing.End(span)
	return nil
}

func (ip *icmpProxy) Serve(ctx context.Context) error {
	ip.srcFunnelTracker.ScheduleCleanup(ctx, ip.idleTimeout)
	return ctx.Err()
}

func (ip *icmpProxy) listenResponse(ctx context.Context, flow *icmpEchoFlow) error {
	buf := make([]byte, mtu)
	for {
		retryable, err := ip.handleResponse(ctx, flow, buf)
		if err != nil && !retryable {
			return err
		}
	}
}

func (ip *icmpProxy) handleResponse(ctx context.Context, flow *icmpEchoFlow, buf []byte) (retryableErr bool, err error) {
	_, span := flow.responder.replySpan(ctx, ip.logger)
	defer flow.responder.exportSpan()

	span.SetAttributes(
		attribute.Int("originalEchoID", flow.originalEchoID),
	)

	n, from, err := flow.originConn.ReadFrom(buf)
	if err != nil {
		tracing.EndWithErrorStatus(span, err)
		return false, err
	}
	reply, err := parseReply(from, buf[:n])
	if err != nil {
		ip.logger.Error().Err(err).Str("dst", from.String()).Msg("Failed to parse ICMP reply")
		tracing.EndWithErrorStatus(span, err)
		return true, err
	}
	if !isEchoReply(reply.msg) {
		err := fmt.Errorf("Expect ICMP echo reply, got %s", reply.msg.Type)
		ip.logger.Debug().Str("dst", from.String()).Msgf("Drop ICMP %s from reply", reply.msg.Type)
		tracing.EndWithErrorStatus(span, err)
		return true, err
	}
	span.SetAttributes(
		attribute.String("dst", reply.from.String()),
		attribute.Int("echoID", reply.echo.ID),
		attribute.Int("seq", reply.echo.Seq),
	)
	if err := flow.returnToSrc(reply); err != nil {
		ip.logger.Err(err).Str("dst", from.String()).Msg("Failed to send ICMP reply")
		tracing.EndWithErrorStatus(span, err)
		return true, err
	}
	tracing.End(span)
	return true, nil
}

// Only linux uses flow3Tuple as FunnelID
func (ft flow3Tuple) Type() string {
	return "srcIP_dstIP_echoID"
}

func (ft flow3Tuple) String() string {
	return fmt.Sprintf("%s:%s:%d", ft.srcIP, ft.dstIP, ft.originalEchoID)
}
