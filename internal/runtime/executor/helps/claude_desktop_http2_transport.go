package helps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	tls "github.com/refraction-networking/utls"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/net/proxy"
)

const (
	claudeDesktopHTTP2DefaultWindow = int64(65535)
	claudeDesktopHTTP2DefaultFrame  = uint32(16384)
)

// claudeDesktopHTTP2Transport is the renderer event-logger transport. It keeps
// one account-scoped Chromium-style HTTP/2 connection so HPACK state, stream
// IDs, TLS resumption, and connection reuse never cross credential boundaries.
// Concurrent requests share that connection as independent HTTP/2 streams.
type claudeDesktopHTTP2Transport struct {
	profile      claudeprofile.TransportProfile
	policy       claudeDesktopRequestPolicy
	dialer       proxy.ContextDialer
	sessionCache tls.ClientSessionCache

	mu         sync.Mutex
	connection *claudeDesktopHTTP2Connection
	closed     bool
}

type claudeDesktopHTTP2Connection struct {
	conn   net.Conn
	framer *http2.Framer

	writeMu       sync.Mutex
	encoderBuffer bytes.Buffer
	encoder       *hpack.Encoder

	stateMu              sync.Mutex
	decoder              *hpack.Decoder
	streams              map[uint32]*claudeDesktopHTTP2ResponseState
	nextStreamID         uint32
	peerInitialWindow    int64
	connectionSendWindow int64
	maxFrameSize         uint32
	windowChanged        chan struct{}
	goAway               bool
	closedErr            error
}

type claudeDesktopHTTP2ResponseState struct {
	streamID        uint32
	statusCode      int
	headers         http.Header
	body            bytes.Buffer
	headerBlock     bytes.Buffer
	headerEndStream bool
	sendWindow      int64
	ended           bool
	completed       bool
	err             error
	done            chan struct{}
}

func newClaudeDesktopHTTP2Transport(profile claudeprofile.TransportProfile, policy claudeDesktopRequestPolicy, proxyURL string) (*claudeDesktopHTTP2Transport, error) {
	if profile.Protocol != "http/2" || profile.ClientHelloPreset != "chromium-148-v140609" {
		return nil, fmt.Errorf("claude desktop renderer transport profile is unsupported")
	}
	if len(profile.HeaderOrder) == 0 || len(profile.HTTP2Settings) == 0 || profile.ConnectionWindow == 0 {
		return nil, fmt.Errorf("claude desktop renderer transport profile is incomplete")
	}
	if errPolicy := policy.validateDefinition(); errPolicy != nil {
		return nil, errPolicy
	}
	dialer, errDialer := claudeDesktopDialer(proxyURL)
	if errDialer != nil {
		return nil, errDialer
	}
	return &claudeDesktopHTTP2Transport{
		profile:      profile,
		policy:       policy,
		dialer:       dialer,
		sessionCache: tls.NewLRUClientSessionCache(64),
	}, nil
}

func (t *claudeDesktopHTTP2Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("claude desktop renderer transport received a nil request")
	}
	if errPolicy := t.policy.validateRequest(request); errPolicy != nil {
		return nil, errPolicy
	}
	return t.roundTripWithProfile(request, t.profile)
}

func (t *claudeDesktopHTTP2Transport) roundTripWithProfile(request *http.Request, profile claudeprofile.TransportProfile) (*http.Response, error) {
	body, errBody := readClaudeDesktopRequestBody(request)
	if errBody != nil {
		return nil, errBody
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("claude desktop renderer transport is closed")
	}
	connection := t.connection
	if connection == nil || !connection.acceptsNewStreams() {
		var errConnect error
		connection, errConnect = t.connect(request.Context(), request.URL.Hostname(), claudeDesktopAddress(request.URL.Hostname(), request.URL.Port()))
		if errConnect != nil {
			t.mu.Unlock()
			return nil, errConnect
		}
		t.connection = connection
	}
	t.mu.Unlock()
	response, errRoundTrip := connection.roundTrip(request, body, profile)
	if errRoundTrip != nil && !connection.acceptsNewStreams() {
		t.mu.Lock()
		if t.connection == connection {
			t.connection = nil
		}
		t.mu.Unlock()
	}
	return response, errRoundTrip
}

func (t *claudeDesktopHTTP2Transport) connect(ctx context.Context, hostname, address string) (*claudeDesktopHTTP2Connection, error) {
	rawConnection, errDial := t.dialer.DialContext(ctx, "tcp", address)
	if errDial != nil {
		return nil, fmt.Errorf("dial Claude Desktop renderer upstream: %w", errDial)
	}
	tlsConfig := &tls.Config{
		ServerName:                         hostname,
		ClientSessionCache:                 t.sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
	tlsConnection := tls.UClient(rawConnection, tlsConfig, tls.HelloCustom)
	clientHello, errClientHello := claudeDesktopRendererClientHelloSpec(hostname)
	if errClientHello != nil {
		_ = rawConnection.Close()
		return nil, errClientHello
	}
	if errPreset := tlsConnection.ApplyPreset(clientHello); errPreset != nil {
		_ = rawConnection.Close()
		return nil, fmt.Errorf("apply Claude Desktop renderer ClientHello profile: %w", errPreset)
	}
	if errHandshake := tlsConnection.HandshakeContext(ctx); errHandshake != nil {
		_ = rawConnection.Close()
		return nil, fmt.Errorf("Claude Desktop renderer TLS handshake: %w", errHandshake)
	}
	if negotiated := tlsConnection.ConnectionState().NegotiatedProtocol; negotiated != "h2" {
		_ = tlsConnection.Close()
		return nil, fmt.Errorf("Claude Desktop renderer transport negotiated %q instead of h2", negotiated)
	}
	framer := http2.NewFramer(tlsConnection, tlsConnection)
	connection := &claudeDesktopHTTP2Connection{
		conn:                 tlsConnection,
		framer:               framer,
		streams:              make(map[uint32]*claudeDesktopHTTP2ResponseState),
		nextStreamID:         1,
		peerInitialWindow:    claudeDesktopHTTP2DefaultWindow,
		connectionSendWindow: claudeDesktopHTTP2DefaultWindow,
		maxFrameSize:         claudeDesktopHTTP2DefaultFrame,
		windowChanged:        make(chan struct{}),
	}
	connection.encoder = hpack.NewEncoder(&connection.encoderBuffer)
	connection.decoder = hpack.NewDecoder(65536, nil)
	connection.decoder.SetAllowedMaxDynamicTableSize(65536)

	if errPreface := writeClaudeDesktopHTTP2ConnectionPreface(tlsConnection, framer, t.profile); errPreface != nil {
		_ = tlsConnection.Close()
		return nil, errPreface
	}
	go connection.readLoop()
	return connection, nil
}

// claudeDesktopRendererClientHelloSpec reproduces the v140609 Chromium 148
// renderer structure captured on Win2. GREASE values and extension order stay
// per-connection dynamic, matching Chromium rather than freezing one packet.
func claudeDesktopRendererClientHelloSpec(hostname string) (*tls.ClientHelloSpec, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil, fmt.Errorf("Claude Desktop renderer ClientHello hostname is empty")
	}
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.GREASE_PLACEHOLDER,
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: tls.ShuffleChromeTLSExtensions([]tls.TLSExtension{
			&tls.UtlsGREASEExtension{},
			&tls.SNIExtension{ServerName: hostname},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{
				tls.CurveID(tls.GREASE_PLACEHOLDER), tls.X25519MLKEM768, tls.X25519, tls.CurveP256, tls.CurveP384,
			}},
			&tls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&tls.SessionTicketExtension{},
			&tls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&tls.StatusRequestExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256, tls.PKCS1WithSHA256,
				tls.ECDSAWithP384AndSHA384, tls.PSSWithSHA384, tls.PKCS1WithSHA384,
				tls.PSSWithSHA512, tls.PKCS1WithSHA512,
			}},
			&tls.SCTExtension{},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.CurveID(tls.GREASE_PLACEHOLDER), Data: []byte{0}},
				{Group: tls.X25519MLKEM768},
				{Group: tls.X25519},
			}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.GREASE_PLACEHOLDER, tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsCompressCertExtension{Algorithms: []tls.CertCompressionAlgo{tls.CertCompressionBrotli}},
			&tls.ApplicationSettingsExtensionNew{SupportedProtocols: []string{"h2"}},
			tls.BoringGREASEECH(),
			&tls.UtlsGREASEExtension{},
		}),
		TLSVersMin: tls.VersionTLS12,
		TLSVersMax: tls.VersionTLS13,
	}, nil
}

func writeClaudeDesktopHTTP2ConnectionPreface(writer io.Writer, framer *http2.Framer, profile claudeprofile.TransportProfile) error {
	if errPreface := writeAll(writer, []byte(http2.ClientPreface)); errPreface != nil {
		return fmt.Errorf("write Claude Desktop renderer HTTP/2 preface: %w", errPreface)
	}
	settings := make([]http2.Setting, 0, len(profile.HTTP2Settings))
	for _, setting := range profile.HTTP2Settings {
		settings = append(settings, http2.Setting{ID: http2.SettingID(setting.ID), Val: setting.Value})
	}
	if errSettings := framer.WriteSettings(settings...); errSettings != nil {
		return fmt.Errorf("write Claude Desktop renderer HTTP/2 settings: %w", errSettings)
	}
	if errWindow := framer.WriteWindowUpdate(0, profile.ConnectionWindow); errWindow != nil {
		return fmt.Errorf("write Claude Desktop renderer HTTP/2 connection window: %w", errWindow)
	}
	return nil
}

func (t *claudeDesktopHTTP2Transport) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.closed = true
	connection := t.connection
	t.connection = nil
	t.mu.Unlock()
	if connection != nil && connection.conn != nil {
		connection.failConnection(fmt.Errorf("claude desktop renderer transport is closed"))
	}
}

func (c *claudeDesktopHTTP2Connection) roundTrip(request *http.Request, body []byte, profile claudeprofile.TransportProfile) (*http.Response, error) {
	responseState, errOpen := c.openStream()
	if errOpen != nil {
		return nil, errOpen
	}
	fields, errFields := claudeDesktopHTTP2HeaderFieldsForProfile(request, body, profile)
	if errFields != nil {
		c.finishStream(responseState.streamID, errFields)
		return nil, errFields
	}
	if errHeaders := c.writeRequestHeaders(responseState.streamID, len(body) == 0, fields); errHeaders != nil {
		c.failConnection(errHeaders)
		return nil, errHeaders
	}
	if errBody := c.sendRequestBody(request.Context(), responseState, body); errBody != nil {
		return nil, errBody
	}
	select {
	case <-responseState.done:
	case <-request.Context().Done():
		c.cancelStream(responseState.streamID, request.Context().Err())
		<-responseState.done
	}
	if responseState.err != nil {
		return nil, responseState.err
	}
	if responseState.statusCode == 0 {
		return nil, fmt.Errorf("Claude Desktop renderer upstream omitted HTTP/2 :status")
	}
	statusText := http.StatusText(responseState.statusCode)
	status := strconv.Itoa(responseState.statusCode)
	if statusText != "" {
		status += " " + statusText
	}
	contentLength := int64(-1)
	if value := strings.TrimSpace(responseState.headers.Get("Content-Length")); value != "" {
		if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
			contentLength = parsed
		}
	}
	return &http.Response{
		Status:        status,
		StatusCode:    responseState.statusCode,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ProtoMinor:    0,
		Header:        responseState.headers,
		Body:          io.NopCloser(bytes.NewReader(responseState.body.Bytes())),
		ContentLength: contentLength,
		Request:       request,
	}, nil
}

func (c *claudeDesktopHTTP2Connection) acceptsNewStreams() bool {
	if c == nil {
		return false
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closedErr == nil && !c.goAway && c.nextStreamID > 0 && c.nextStreamID <= (1<<31)-1
}

func (c *claudeDesktopHTTP2Connection) openStream() (*claudeDesktopHTTP2ResponseState, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closedErr != nil {
		return nil, c.closedErr
	}
	if c.goAway {
		return nil, fmt.Errorf("Claude Desktop renderer HTTP/2 connection received GOAWAY")
	}
	if c.nextStreamID == 0 || c.nextStreamID > (1<<31)-1 {
		return nil, fmt.Errorf("Claude Desktop renderer HTTP/2 stream IDs are exhausted")
	}
	streamID := c.nextStreamID
	c.nextStreamID += 2
	state := &claudeDesktopHTTP2ResponseState{
		streamID:   streamID,
		headers:    make(http.Header),
		sendWindow: c.peerInitialWindow,
		done:       make(chan struct{}),
	}
	c.streams[streamID] = state
	return state, nil
}

func (c *claudeDesktopHTTP2Connection) writeRequestHeaders(streamID uint32, endStream bool, fields []hpack.HeaderField) error {
	c.stateMu.Lock()
	maximum := c.maxFrameSize
	c.stateMu.Unlock()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.encoderBuffer.Reset()
	for _, field := range fields {
		if errEncode := c.encoder.WriteField(field); errEncode != nil {
			return fmt.Errorf("encode Claude Desktop renderer header %q: %w", field.Name, errEncode)
		}
	}
	headerBlock := append([]byte(nil), c.encoderBuffer.Bytes()...)
	return c.writeHeadersLocked(streamID, endStream, headerBlock, maximum)
}

func (c *claudeDesktopHTTP2Connection) sendRequestBody(ctx context.Context, state *claudeDesktopHTTP2ResponseState, body []byte) error {
	for offset := 0; offset < len(body); {
		available, changed, errReserve := c.reserveSendWindow(state, int64(len(body)-offset))
		if errReserve != nil {
			return errReserve
		}
		if available == 0 {
			select {
			case <-ctx.Done():
				c.cancelStream(state.streamID, ctx.Err())
				return ctx.Err()
			case <-state.done:
				if state.err != nil {
					return state.err
				}
				return fmt.Errorf("Claude Desktop renderer upstream ended the response before the request body completed")
			case <-changed:
				continue
			}
		}
		select {
		case <-ctx.Done():
			c.restoreSendWindow(state, available)
			c.cancelStream(state.streamID, ctx.Err())
			return ctx.Err()
		default:
		}
		endStream := offset+int(available) == len(body)
		c.writeMu.Lock()
		errData := c.framer.WriteData(state.streamID, endStream, body[offset:offset+int(available)])
		c.writeMu.Unlock()
		if errData != nil {
			errData = fmt.Errorf("write Claude Desktop renderer HTTP/2 body: %w", errData)
			c.failConnection(errData)
			return errData
		}
		offset += int(available)
	}
	return nil
}

func (c *claudeDesktopHTTP2Connection) reserveSendWindow(state *claudeDesktopHTTP2ResponseState, remaining int64) (int64, <-chan struct{}, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closedErr != nil {
		return 0, nil, c.closedErr
	}
	if state.completed {
		if state.err != nil {
			return 0, nil, state.err
		}
		return 0, nil, fmt.Errorf("Claude Desktop renderer upstream ended the response before the request body completed")
	}
	available := c.connectionSendWindow
	if state.sendWindow < available {
		available = state.sendWindow
	}
	if int64(c.maxFrameSize) < available {
		available = int64(c.maxFrameSize)
	}
	if remaining < available {
		available = remaining
	}
	if available <= 0 {
		return 0, c.windowChanged, nil
	}
	c.connectionSendWindow -= available
	state.sendWindow -= available
	return available, c.windowChanged, nil
}

func (c *claudeDesktopHTTP2Connection) restoreSendWindow(state *claudeDesktopHTTP2ResponseState, amount int64) {
	if amount <= 0 {
		return
	}
	c.stateMu.Lock()
	c.connectionSendWindow += amount
	if !state.completed {
		state.sendWindow += amount
	}
	c.signalWindowChangedLocked()
	c.stateMu.Unlock()
}

func (c *claudeDesktopHTTP2Connection) writeHeadersLocked(streamID uint32, endStream bool, block []byte, maximumFrameSize uint32) error {
	maximum := int(maximumFrameSize)
	if maximum <= 5 {
		maximum = int(claudeDesktopHTTP2DefaultFrame)
	}
	firstLength := len(block)
	if firstLength > maximum-5 {
		firstLength = maximum - 5
	}
	endHeaders := firstLength == len(block)
	if errHeaders := c.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:firstLength],
		EndStream:     endStream,
		EndHeaders:    endHeaders,
		Priority:      http2.PriorityParam{StreamDep: 0, Weight: 255},
	}); errHeaders != nil {
		return fmt.Errorf("write Claude Desktop renderer HTTP/2 headers: %w", errHeaders)
	}
	for offset := firstLength; offset < len(block); {
		end := offset + maximum
		if end > len(block) {
			end = len(block)
		}
		if errContinuation := c.framer.WriteContinuation(streamID, end == len(block), block[offset:end]); errContinuation != nil {
			return fmt.Errorf("write Claude Desktop renderer HTTP/2 continuation: %w", errContinuation)
		}
		offset = end
	}
	return nil
}

func (c *claudeDesktopHTTP2Connection) readLoop() {
	for {
		frame, errRead := c.framer.ReadFrame()
		if errRead != nil {
			c.failConnection(fmt.Errorf("read Claude Desktop renderer HTTP/2 frame: %w", errRead))
			return
		}
		if errFrame := c.handleFrame(frame); errFrame != nil {
			c.failConnection(errFrame)
			return
		}
	}
}

func (c *claudeDesktopHTTP2Connection) handleFrame(frame http2.Frame) error {
	switch typed := frame.(type) {
	case *http2.SettingsFrame:
		if typed.IsAck() {
			return nil
		}
		var headerTableSize *uint32
		c.stateMu.Lock()
		for index := 0; index < typed.NumSettings(); index++ {
			setting := typed.Setting(index)
			switch setting.ID {
			case http2.SettingHeaderTableSize:
				value := setting.Val
				headerTableSize = &value
			case http2.SettingInitialWindowSize:
				delta := int64(setting.Val) - c.peerInitialWindow
				c.peerInitialWindow = int64(setting.Val)
				for _, state := range c.streams {
					state.sendWindow += delta
				}
			case http2.SettingMaxFrameSize:
				c.maxFrameSize = setting.Val
			}
		}
		c.signalWindowChangedLocked()
		c.stateMu.Unlock()
		c.writeMu.Lock()
		if headerTableSize != nil {
			c.encoder.SetMaxDynamicTableSize(*headerTableSize)
		}
		errAck := c.framer.WriteSettingsAck()
		c.writeMu.Unlock()
		if errAck != nil {
			return fmt.Errorf("acknowledge Claude Desktop renderer HTTP/2 settings: %w", errAck)
		}
	case *http2.WindowUpdateFrame:
		c.stateMu.Lock()
		if typed.StreamID == 0 {
			c.connectionSendWindow += int64(typed.Increment)
		} else if state := c.streams[typed.StreamID]; state != nil {
			state.sendWindow += int64(typed.Increment)
		}
		c.signalWindowChangedLocked()
		c.stateMu.Unlock()
	case *http2.PingFrame:
		if typed.IsAck() {
			return nil
		}
		c.writeMu.Lock()
		errPing := c.framer.WritePing(true, typed.Data)
		c.writeMu.Unlock()
		if errPing != nil {
			return fmt.Errorf("acknowledge Claude Desktop renderer HTTP/2 ping: %w", errPing)
		}
	case *http2.HeadersFrame:
		c.stateMu.Lock()
		state := c.streams[typed.StreamID]
		if state != nil {
			_, _ = state.headerBlock.Write(typed.HeaderBlockFragment())
			state.headerEndStream = typed.StreamEnded()
			if typed.HeadersEnded() {
				if errHeaders := c.finishResponseHeadersLocked(state); errHeaders != nil {
					c.stateMu.Unlock()
					return errHeaders
				}
				if state.ended {
					c.finishStreamLocked(state, nil)
				}
			}
		}
		c.stateMu.Unlock()
	case *http2.ContinuationFrame:
		c.stateMu.Lock()
		state := c.streams[typed.StreamID]
		if state != nil {
			_, _ = state.headerBlock.Write(typed.HeaderBlockFragment())
			if typed.HeadersEnded() {
				if errHeaders := c.finishResponseHeadersLocked(state); errHeaders != nil {
					c.stateMu.Unlock()
					return errHeaders
				}
				if state.ended {
					c.finishStreamLocked(state, nil)
				}
			}
		}
		c.stateMu.Unlock()
	case *http2.DataFrame:
		data := typed.Data()
		c.stateMu.Lock()
		state := c.streams[typed.StreamID]
		if state != nil {
			_, _ = state.body.Write(data)
			state.ended = typed.StreamEnded()
		}
		c.stateMu.Unlock()
		if len(data) > 0 {
			increment := uint32(len(data))
			c.writeMu.Lock()
			errConnectionWindow := c.framer.WriteWindowUpdate(0, increment)
			var errStreamWindow error
			if errConnectionWindow == nil && state != nil {
				errStreamWindow = c.framer.WriteWindowUpdate(typed.StreamID, increment)
			}
			c.writeMu.Unlock()
			if errConnectionWindow != nil {
				return fmt.Errorf("restore Claude Desktop renderer HTTP/2 connection window: %w", errConnectionWindow)
			}
			if errStreamWindow != nil {
				return fmt.Errorf("restore Claude Desktop renderer HTTP/2 stream window: %w", errStreamWindow)
			}
		}
		if state != nil && typed.StreamEnded() {
			c.finishStream(typed.StreamID, nil)
		}
	case *http2.RSTStreamFrame:
		c.finishStream(typed.StreamID, fmt.Errorf("Claude Desktop renderer upstream reset stream with %s", typed.ErrCode))
	case *http2.GoAwayFrame:
		c.stateMu.Lock()
		c.goAway = true
		for streamID, state := range c.streams {
			if streamID > typed.LastStreamID {
				c.finishStreamLocked(state, fmt.Errorf("Claude Desktop renderer upstream closed the HTTP/2 connection with %s", typed.ErrCode))
			}
		}
		c.stateMu.Unlock()
	}
	return nil
}

func (c *claudeDesktopHTTP2Connection) finishResponseHeadersLocked(state *claudeDesktopHTTP2ResponseState) error {
	fields, errDecode := c.decoder.DecodeFull(state.headerBlock.Bytes())
	state.headerBlock.Reset()
	if errDecode != nil {
		return fmt.Errorf("decode Claude Desktop renderer HTTP/2 response headers: %w", errDecode)
	}
	for _, field := range fields {
		if field.Name == ":status" {
			status, errStatus := strconv.Atoi(field.Value)
			if errStatus != nil {
				return fmt.Errorf("decode Claude Desktop renderer HTTP/2 status: %w", errStatus)
			}
			state.statusCode = status
			continue
		}
		if strings.HasPrefix(field.Name, ":") {
			continue
		}
		state.headers.Add(field.Name, field.Value)
	}
	if state.headerEndStream {
		state.ended = true
	}
	state.headerEndStream = false
	return nil
}

func (c *claudeDesktopHTTP2Connection) cancelStream(streamID uint32, cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	c.writeMu.Lock()
	errReset := c.framer.WriteRSTStream(streamID, http2.ErrCodeCancel)
	c.writeMu.Unlock()
	if errReset != nil {
		c.failConnection(fmt.Errorf("cancel Claude Desktop renderer HTTP/2 stream: %w", errReset))
		return
	}
	c.finishStream(streamID, cause)
}

func (c *claudeDesktopHTTP2Connection) finishStream(streamID uint32, err error) {
	c.stateMu.Lock()
	if state := c.streams[streamID]; state != nil {
		c.finishStreamLocked(state, err)
	}
	c.stateMu.Unlock()
}

func (c *claudeDesktopHTTP2Connection) finishStreamLocked(state *claudeDesktopHTTP2ResponseState, err error) {
	if state == nil || state.completed {
		return
	}
	state.err = err
	state.completed = true
	delete(c.streams, state.streamID)
	close(state.done)
	c.signalWindowChangedLocked()
}

func (c *claudeDesktopHTTP2Connection) failConnection(err error) {
	if c == nil {
		return
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	c.stateMu.Lock()
	if c.closedErr != nil {
		c.stateMu.Unlock()
		return
	}
	c.closedErr = err
	c.goAway = true
	for _, state := range c.streams {
		c.finishStreamLocked(state, err)
	}
	c.signalWindowChangedLocked()
	c.stateMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

func (c *claudeDesktopHTTP2Connection) signalWindowChangedLocked() {
	if c.windowChanged == nil {
		c.windowChanged = make(chan struct{})
		return
	}
	close(c.windowChanged)
	c.windowChanged = make(chan struct{})
}

func claudeDesktopHTTP2HeaderFields(request *http.Request, body []byte, order []string) ([]hpack.HeaderField, error) {
	return claudeDesktopHTTP2HeaderFieldsForProfile(request, body, claudeprofile.TransportProfile{HeaderOrder: order})
}

func claudeDesktopHTTP2HeaderFieldsForProfile(request *http.Request, body []byte, profile claudeprofile.TransportProfile) ([]hpack.HeaderField, error) {
	order := profile.HeaderOrder
	fields := make([]hpack.HeaderField, 0, len(order))
	known := make(map[string]struct{}, len(order))
	optional := claudeDesktopOptionalHeaders(profile.OptionalHeaders)
	for _, rawName := range order {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if name == "" {
			return nil, fmt.Errorf("Claude Desktop renderer profile contains an empty header name")
		}
		known[name] = struct{}{}
		var values []string
		switch name {
		case ":method":
			values = []string{request.Method}
		case ":authority":
			authority := strings.TrimSpace(request.Host)
			if authority == "" {
				authority = request.URL.Host
			}
			values = []string{authority}
		case ":scheme":
			values = []string{request.URL.Scheme}
		case ":path":
			path := request.URL.RequestURI()
			if path == "" {
				path = "/"
			}
			values = []string{path}
		case "content-length":
			values = []string{strconv.Itoa(len(body))}
		default:
			values = claudeDesktopHeaderValues(request.Header, name)
		}
		if len(values) == 0 {
			if _, ok := optional[name]; ok {
				continue
			}
			return nil, fmt.Errorf("Claude Desktop renderer request omits profiled header %q", name)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("Claude Desktop renderer header %q contains a line break", name)
			}
			fields = append(fields, hpack.HeaderField{Name: name, Value: value})
		}
	}
	for rawName := range request.Header {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("Claude Desktop renderer request contains unprofiled header %q", rawName)
		}
	}
	return fields, nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, errWrite := writer.Write(payload)
		if errWrite != nil {
			return errWrite
		}
		if written <= 0 {
			return io.ErrUnexpectedEOF
		}
		payload = payload[written:]
	}
	return nil
}
