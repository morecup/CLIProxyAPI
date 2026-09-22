package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const factGrowthbookExposure = "sdk-growthbook-exposure"

// ObserveSDKFeatureExposure acknowledges durable queue acceptance, not HTTP
// delivery. A configured but unavailable logger must allow the next real read
// to retry. No batch configuration acknowledges without inventing an event.
func (m *Manager) ObserveSDKFeatureExposure(auth *cliproxyauth.Auth, exposure features.Exposure) (bool, error) {
	if m == nil || m.bundle == nil || m.sdkProfile.SchemaVersion == 0 || m.sdkDelivery.batch.MaxPendingEvents == 0 {
		return true, nil
	}
	if !m.Enabled() || m.ctx.Err() != nil {
		return false, fmt.Errorf("Claude Desktop SDK exposure logger is unavailable")
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		m.setEndpointState(m.sdkDelivery.endpointRole, "awaiting-sdk-feature-facts", "SDK feature exposure queue is unavailable")
		return false, err
	}
	at := m.now().UTC()
	id := uuid.NewString()
	payload := projectGrowthbookExposure(worker.binding, exposure, m.bundle.CodeVersion, id, at)
	err = worker.enqueue(Envelope{
		Version: 1, EventUUID: id, EndpointRole: m.sdkDelivery.endpointRole,
		CatalogFact: factGrowthbookExposure, OccurredAt: at.Format(time.RFC3339Nano),
		Binding: worker.binding, SessionID: exposure.SessionID, Payload: payload,
	})
	errIssue := worker.setFactIssue(factIssueSDKExposure, featureIssueSession(exposure.SessionID), exposure.FeatureID, err != nil)
	if err != nil || errIssue != nil {
		worker.recordQueueFailure(errors.Join(err, errIssue))
	}
	// A successfully persisted event must not be duplicated merely because its
	// independent diagnostic ledger failed to clear an older issue.
	return err == nil, errors.Join(err, errIssue)
}

// ObserveSDKFeatureState keeps cache/refresh errors visible across successful
// model requests and restarts. Only a matching healthy feature operation clears
// the issue; unrelated request success cannot erase it.
func (m *Manager) ObserveSDKFeatureState(auth *cliproxyauth.Auth, session string, healthy bool) error {
	if m == nil || !m.Enabled() || m.ctx.Err() != nil {
		return nil
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		m.setEndpointState(m.sdkDelivery.endpointRole, "awaiting-sdk-feature-facts", "SDK feature cache or refresh state is unavailable")
		return err
	}
	err = worker.setFactIssue(factIssueSDKFeatures, featureIssueSession(session), "feature-service", !healthy)
	if err != nil {
		worker.recordQueueFailure(err)
	}
	return err
}

func featureIssueSession(session string) string {
	if session == "" {
		return "sdk-host"
	}
	return session
}

// SDKFeatureHostObserver binds live health to one query instance and one
// existing account worker. Retirement is not a successful fetch, disk repair
// or upstream event. These faults expire with the process; durable cache,
// alias and exposure failures keep their separate protected ledger entries.
func (m *Manager) SDKFeatureHostObserver(auth *cliproxyauth.Auth, hostID string) (func(bool) error, func(), error) {
	if m == nil || !m.Enabled() {
		return func(bool) error { return nil }, func() {}, nil
	}
	if m.ctx.Err() != nil || hostID == "" {
		return nil, nil, fmt.Errorf("Claude Desktop SDK health owner is unavailable")
	}
	worker, err := m.workerForDelivery(auth, m.sdkDelivery)
	if err != nil {
		m.setEndpointState(m.sdkDelivery.endpointRole, "awaiting-sdk-feature-facts", "SDK query health observer is unavailable")
		return nil, nil, err
	}
	retired := false // Protected by worker.factIssueMu, including late callbacks.
	observe := func(healthy bool) error {
		worker.factIssueMu.Lock()
		defer worker.factIssueMu.Unlock()
		if retired || m.ctx.Err() != nil {
			return nil
		}
		if healthy {
			delete(worker.featureHostIssues, hostID)
		} else {
			if worker.featureHostIssues == nil {
				worker.featureHostIssues = make(map[string]bool)
			}
			worker.featureHostIssues[hostID] = true
		}
		return nil
	}
	retire := func() {
		worker.factIssueMu.Lock()
		defer worker.factIssueMu.Unlock()
		retired = true
		delete(worker.featureHostIssues, hostID)
	}
	return observe, retire, nil
}

// This is a separate generated-codec shape, not sdkEventData. In particular its
// metadata fields are JSON strings, its timestamp is not client_timestamp, and
// event_name/model/betas/process/entrypoint are absent. It has no Datadog mirror.
func projectGrowthbookExposure(binding Binding, exposure features.Exposure, version, eventID string, at time.Time) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"event_type":"GrowthbookExperimentEvent","event_data":{`)
	field := func(name, value string) {
		b.WriteString(nativeJSONString(name))
		b.WriteByte(':')
		b.WriteString(nativeJSONString(value))
		b.WriteByte(',')
	}
	field("event_id", eventID)
	field("timestamp", at.UTC().Format("2006-01-02T15:04:05.000Z"))
	field("experiment_id", exposure.ExperimentID)
	b.WriteString(`"variation_id":`)
	variation := nativeRoundVariation(exposure.VariationID)
	if math.IsNaN(variation) || math.IsInf(variation, 0) {
		b.WriteString("null")
	} else {
		if variation == 0 {
			variation = 0
		}
		encoded, _ := json.Marshal(variation)
		b.Write(encoded)
	}
	b.WriteByte(',')
	field("environment", "production")
	if version != "" {
		field("user_attributes", `{"appVersion":`+nativeJSONString(version)+`}`)
	}
	field("experiment_metadata", `{"feature_id":`+nativeJSONString(exposure.FeatureID)+`}`)
	if binding.DeviceID != "" {
		field("device_id", claudedesktop.RequestDeviceID(binding.DeviceID))
	}
	if exposure.SessionID != "" {
		field("session_id", exposure.SessionID)
	}
	if binding.AccountUUID != "" || binding.OrganizationUUID != "" {
		b.WriteString(`"auth":{`)
		if binding.AccountUUID != "" {
			field("account_uuid", binding.AccountUUID)
		}
		if binding.OrganizationUUID != "" {
			field("organization_uuid", binding.OrganizationUUID)
		}
		text := b.String()
		b.Reset()
		b.WriteString(strings.TrimSuffix(text, ","))
		b.WriteString("},")
	}
	return json.RawMessage(strings.TrimSuffix(b.String(), ",") + "}}")
}

func nativeRoundVariation(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1<<52 || value == 0 {
		return value
	}
	floor := math.Floor(value)
	if value-floor >= 0.5 {
		return floor + 1
	}
	return floor
}

// JSON.stringify does not HTML-escape strings or escape U+2028/U+2029. Keeping
// this small string writer avoids introducing a generic object serializer just
// for the two fixed native metadata objects.
func nativeJSONString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteString(fmt.Sprintf("%02x", r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Keep the factory's serialized bytes through encrypted persistence. Encoding
// RawMessage with encoding/json would otherwise HTML-escape and rewrite its
// string bytes before a later delivery, even though decoded fields still match.
func marshalGrowthbookQueueEnvelope(envelope Envelope) ([]byte, error) {
	if !json.Valid(envelope.Payload) {
		return nil, fmt.Errorf("invalid SDK exposure payload")
	}
	type metadata Envelope
	outer := struct {
		metadata
		Payload json.RawMessage `json:"payload,omitempty"`
	}{metadata: metadata(envelope)}
	encoded, err := json.Marshal(outer)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded[:len(encoded)-1], `,"payload":`...)
	encoded = append(encoded, envelope.Payload...)
	return append(encoded, '}'), nil
}
