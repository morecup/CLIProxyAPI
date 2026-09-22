package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const protectedRecordVersion = 1

var queueFilePattern = regexp.MustCompile(`^(\d{20})_([0-9a-f-]{36})_a(\d+)_t(\d+)\.(pending|dead|sending_\d+_[0-9a-f]+)$`)

type protectedRecord struct {
	Version    int    `json:"version"`
	Protector  string `json:"protector"`
	Ciphertext string `json:"ciphertext"`
}

type lineageRecord struct {
	Version           int    `json:"version"`
	BindingRevision   string `json:"binding_revision"`
	SessionID         string `json:"session_id"`
	Revision          uint64 `json:"revision"`
	NextSequence      uint64 `json:"next_sequence"`
	CommittedSequence uint64 `json:"committed_sequence"`
	PreviousRequestID string `json:"previous_request_id,omitempty"`
	UpdatedAt         string `json:"updated_at"`
}

type queueFile struct {
	path          string
	name          string
	sequence      uint64
	eventUUID     string
	attempt       int
	nextAttemptAt time.Time
	state         string
}

type workerRuntimeStatus struct {
	consecutiveFailures int
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	lastError           string
	nextAttemptAt       time.Time
	queueWritable       bool
}

type accountWorker struct {
	factIssueMu           sync.Mutex
	factIssues            map[string]string
	featureHostIssues     map[string]bool // Live query faults; durable cache/exposure faults remain in factIssues.
	factIssuesOverflow    bool
	factIssuesUnreadable  bool
	factIssuesWriteFailed bool
	manager               *Manager
	profile               deliveryProfile
	binding               Binding
	directory             string
	sessionDir            string
	workerID              string
	wake                  chan struct{}
	flushMu               sync.Mutex
	sessionMu             sync.Mutex
	inputJournalTainted   map[string]bool // Guarded by sessionMu; never retry a lost index as known.
	inputJournalOverflow  bool
	mu                    sync.Mutex
	auth                  *cliproxyauth.Auth
	nextSequence          uint64
	lastAPICall           time.Time
	status                workerRuntimeStatus
}

func (w *accountWorker) observeAPICall(at time.Time) *int64 {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	previous := w.lastAPICall
	w.lastAPICall = at
	if previous.IsZero() {
		return nil
	}
	delta := at.Sub(previous).Milliseconds()
	if delta < 0 {
		delta = 0
	}
	return &delta
}

func (m *Manager) restoreWorkers() error {
	if !m.Enabled() {
		return nil
	}
	telemetryRoot := filepath.Join(m.root, "telemetry")
	entries, errRead := os.ReadDir(telemetryRoot)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read persisted telemetry queues: %w", errRead)
	}
	var joined error
	deliveryByNamespace := map[string]deliveryProfile{m.sdkDelivery.queueNamespace: m.sdkDelivery}
	for _, delivery := range m.auxiliaryDeliveries {
		deliveryByNamespace[delivery.queueNamespace] = delivery
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if isBindingDirectoryName(entry.Name()) {
			joined = errors.Join(joined, m.restoreWorkerDirectory(filepath.Join(telemetryRoot, entry.Name()), entry.Name(), m.rendererDelivery))
			continue
		}
		delivery, knownDelivery := deliveryByNamespace[entry.Name()]
		if !knownDelivery {
			continue
		}
		roleRoot := filepath.Join(telemetryRoot, entry.Name())
		roleEntries, errRoleEntries := os.ReadDir(roleRoot)
		if errRoleEntries != nil {
			joined = errors.Join(joined, errRoleEntries)
			continue
		}
		for _, roleEntry := range roleEntries {
			if !roleEntry.IsDir() || !isBindingDirectoryName(roleEntry.Name()) {
				continue
			}
			joined = errors.Join(joined, m.restoreWorkerDirectory(filepath.Join(roleRoot, roleEntry.Name()), roleEntry.Name(), delivery))
		}
	}
	return joined
}

func (m *Manager) restoreWorkerDirectory(directory, directoryName string, profile deliveryProfile) error {
	binding, found, errBinding := restoredBindingFromDirectory(directory)
	if errBinding != nil {
		m.retainFactRestoreFailure(profile.endpointRole)
	}
	if errBinding != nil || !found {
		return errBinding
	}
	if errValidate := m.validateRestoredBinding(binding, directoryName, profile); errValidate != nil {
		m.retainFactRestoreFailure(profile.endpointRole)
		return errValidate
	}
	worker, errWorker := newAccountWorker(m, binding, profile, nil)
	if errWorker != nil {
		return errWorker
	}
	key := profile.endpointRole + "\x00" + binding.BindingRevision
	m.workers[key] = worker
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		worker.run(m.ctx)
	}()
	return nil
}

func isBindingDirectoryName(name string) bool {
	if len(name) != 24 {
		return false
	}
	_, errDecode := hex.DecodeString(name)
	return errDecode == nil
}

func restoredBindingFromDirectory(directory string) (Binding, bool, error) {
	entries, errRead := os.ReadDir(directory)
	if errRead != nil {
		return Binding{}, false, fmt.Errorf("read persisted telemetry account queue: %w", errRead)
	}
	hadQueueRecord := false
	for _, entry := range entries {
		if entry.IsDir() || len(queueFilePattern.FindStringSubmatch(entry.Name())) != 6 {
			continue
		}
		hadQueueRecord = true
		payload, errPayload := readProtectedFile(filepath.Join(directory, entry.Name()))
		if errPayload != nil {
			continue
		}
		var envelope Envelope
		if errEnvelope := json.Unmarshal(payload, &envelope); errEnvelope != nil || envelope.Version != 1 {
			continue
		}
		return envelope.Binding, true, nil
	}
	if hadQueueRecord {
		return Binding{}, false, fmt.Errorf("persisted telemetry account queue has no readable binding")
	}
	record, errFacts := readFactIssueRecord(directory)
	if errFacts == nil {
		return record.Binding, true, nil
	}
	if !errors.Is(errFacts, os.ErrNotExist) {
		return Binding{}, false, errFacts
	}
	return Binding{}, false, nil
}

func (m *Manager) validateRestoredBinding(binding Binding, directoryName string, profile deliveryProfile) error {
	if m == nil || m.bundle == nil {
		return fmt.Errorf("telemetry profile is unavailable")
	}
	if binding.ProfileID != m.bundle.ProfileID || binding.DesktopVersion != m.bundle.DesktopVersion {
		return fmt.Errorf("persisted telemetry binding targets another Desktop profile")
	}
	wantEgressRevision := scopeRevision("telemetry-egress-v1", binding.EgressProxyURL)
	wantDestinationRevision := scopeRevision("telemetry-destination-v1", strings.TrimSpace(profile.endpoint))
	wantTransportRevision := strings.TrimSpace(profile.transportRevision)
	destinationValid := binding.DestinationRevision == wantDestinationRevision
	if len(profile.runtimeMaterials) > 0 {
		_, errDestination := hex.DecodeString(binding.DestinationRevision)
		destinationValid = len(binding.DestinationRevision) == sha256.Size*2 && errDestination == nil
	}
	if binding.EgressRevision != wantEgressRevision || !destinationValid || binding.TransportRevision != wantTransportRevision {
		return fmt.Errorf("persisted telemetry binding transport scope is invalid")
	}
	digest := bindingDigest(binding.AuthID, binding.AccountUUID, binding.OrganizationUUID, binding.DeviceID, binding.ProfileID, binding.DesktopVersion, binding.EgressRevision, binding.DestinationRevision, binding.TransportRevision)
	wantRevision := hex.EncodeToString(digest[:])
	if binding.BindingRevision != wantRevision || len(wantRevision) < 24 || directoryName != wantRevision[:24] {
		return fmt.Errorf("persisted telemetry binding revision is invalid")
	}
	wantRuntimeUUID := uuid.NewSHA1(uuid.NameSpaceOID, digest[:]).String()
	if binding.RuntimeUUID != wantRuntimeUUID {
		return fmt.Errorf("persisted telemetry runtime identity is invalid")
	}
	return nil
}

func newAccountWorker(manager *Manager, binding Binding, profile deliveryProfile, auth *cliproxyauth.Auth) (*accountWorker, error) {
	if manager == nil || manager.root == "" {
		return nil, fmt.Errorf("telemetry state path is not configured")
	}
	directory := telemetryWorkerDirectory(manager.root, profile, binding.BindingRevision)
	sessionDir := filepath.Join(directory, "sessions")
	if errMkdir := os.MkdirAll(sessionDir, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("create telemetry account state: %w", errMkdir)
	}
	worker := &accountWorker{
		manager:    manager,
		profile:    profile,
		binding:    binding,
		directory:  directory,
		sessionDir: sessionDir,
		workerID:   randomID(),
		wake:       make(chan struct{}, 1),
		auth:       auth,
		status:     workerRuntimeStatus{queueWritable: true},
	}
	worker.loadFactIssues()
	files, errFiles := worker.scanQueue()
	if errFiles != nil {
		return nil, errFiles
	}
	for _, file := range files {
		if file.sequence > worker.nextSequence {
			worker.nextSequence = file.sequence
		}
	}
	if errRecover := worker.recoverStaleClaims(manager.now()); errRecover != nil {
		worker.recordQueueFailure(errRecover)
	}
	return worker, nil
}

func telemetryWorkerDirectory(root string, profile deliveryProfile, bindingRevision string) string {
	base := filepath.Join(root, "telemetry")
	if profile.queueNamespace != "" && profile.queueNamespace != "renderer-event-logging" {
		base = filepath.Join(base, profile.queueNamespace)
	}
	return filepath.Join(base, bindingRevision[:24])
}

func (w *accountWorker) updateAuth(auth *cliproxyauth.Auth) {
	if w == nil || auth == nil {
		return
	}
	w.mu.Lock()
	w.auth = auth
	w.mu.Unlock()
}

func (w *accountWorker) currentDoer() HTTPDoer {
	if factory := w.manager.endpointDoerFactory; factory != nil {
		if doer := factory(w.binding.EgressProxyURL, w.profile.endpointRole, w.authSnapshot()); doer != nil {
			return doer
		}
	}
	factory := w.manager.doerFactory
	if factory == nil {
		factory = defaultHTTPDoer
	}
	if doer := factory(w.binding.EgressProxyURL); doer != nil {
		return doer
	}
	return http.DefaultClient
}

func (w *accountWorker) ensureSessionInitialized(ctx context.Context, facts RequestFacts) (bool, error) {
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	digest := sha256String(rendererRecordKey(facts))
	markerPath := filepath.Join(w.sessionDir, digest+".initialized")
	if _, errStat := os.Stat(markerPath); errStat == nil {
		return false, nil
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return false, errStat
	}
	metadata := map[string]any{
		"session_id":        rendererRecordID(facts),
		"user_message_uuid": facts.PromptID,
		"mcp_server_count":  facts.MCPServerCount,
		"has_worktree":      false,
		"has_launch_tools":  true,
		"is_git_repo":       false,
		"is_ssh":            false,
		"backend_kind":      "local",
		"renderer_surface":  "epitaxy",
		"model":             facts.Model,
		"effort":            "high",
		"effort_source":     "persisted",
	}
	metadata["permission_mode"] = defaultString(facts.PermissionMode, "default")
	w.runDesktopSessionStartHooks(ctx, facts) // D1: native binary preflight precedes the session spawn.
	if errEnqueue := w.enqueueProjected(ctx, FactSessionInitialized, facts.SessionID, facts.ClientRequestID, metadata); errEnqueue != nil {
		return false, errEnqueue
	}
	file, errOpen := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrExist) {
			return false, nil
		}
		return true, errOpen
	}
	if _, errWrite := file.WriteString(w.manager.now().UTC().Format(time.RFC3339Nano)); errWrite != nil {
		_ = file.Close()
		_ = os.Remove(markerPath)
		return true, errWrite
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		_ = os.Remove(markerPath)
		return true, errSync
	}
	if errClose := file.Close(); errClose != nil {
		_ = os.Remove(markerPath)
		return true, errClose
	}
	return true, nil
}

func (w *accountWorker) beginLineage(sessionID string) (*LineageToken, string, error) {
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	record, files, errLoad := w.loadLineage(sessionID)
	if errLoad != nil {
		return nil, "", errLoad
	}
	record.NextSequence++
	record.Revision++
	record.UpdatedAt = w.manager.now().UTC().Format(time.RFC3339Nano)
	path, errWrite := w.writeLineage(record)
	if errWrite != nil {
		return nil, "", errWrite
	}
	_ = removeLineageSnapshots(files, path)
	return &LineageToken{worker: w, sessionID: sessionID, sequence: record.NextSequence}, record.PreviousRequestID, nil
}

func (w *accountWorker) commitLineage(sessionID string, sequence uint64, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if sequence == 0 || !strings.HasPrefix(requestID, "req_") {
		return nil
	}
	w.sessionMu.Lock()
	defer w.sessionMu.Unlock()
	record, files, errLoad := w.loadLineage(sessionID)
	if errLoad != nil {
		return errLoad
	}
	if sequence < record.CommittedSequence {
		return nil
	}
	if sequence > record.NextSequence {
		return fmt.Errorf("telemetry lineage sequence %d exceeds reserved sequence %d", sequence, record.NextSequence)
	}
	record.CommittedSequence = sequence
	record.PreviousRequestID = requestID
	record.Revision++
	record.UpdatedAt = w.manager.now().UTC().Format(time.RFC3339Nano)
	path, errWrite := w.writeLineage(record)
	if errWrite != nil {
		return errWrite
	}
	_ = removeLineageSnapshots(files, path)
	return nil
}

func (w *accountWorker) loadLineage(sessionID string) (lineageRecord, []string, error) {
	digest := sha256String(sessionID)
	prefix := digest + "_"
	entries, errRead := os.ReadDir(w.sessionDir)
	if errRead != nil {
		return lineageRecord{}, nil, errRead
	}
	var files []string
	var latestPath string
	var latestRevision uint64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".lineage") {
			continue
		}
		revisionText := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".lineage")
		revision, errRevision := strconv.ParseUint(revisionText, 10, 64)
		if errRevision != nil {
			continue
		}
		path := filepath.Join(w.sessionDir, entry.Name())
		files = append(files, path)
		if latestPath == "" || revision > latestRevision {
			latestPath = path
			latestRevision = revision
		}
	}
	if latestPath == "" {
		return lineageRecord{
			Version:         1,
			BindingRevision: w.binding.BindingRevision,
			SessionID:       sessionID,
		}, files, nil
	}
	payload, errPayload := readProtectedFile(latestPath)
	if errPayload != nil {
		return lineageRecord{}, files, fmt.Errorf("read Claude Desktop session lineage: %w", errPayload)
	}
	var record lineageRecord
	if errUnmarshal := json.Unmarshal(payload, &record); errUnmarshal != nil {
		return lineageRecord{}, files, fmt.Errorf("decode Claude Desktop session lineage: %w", errUnmarshal)
	}
	if record.Version != 1 || record.BindingRevision != w.binding.BindingRevision || record.SessionID != sessionID || record.Revision != latestRevision {
		return lineageRecord{}, files, fmt.Errorf("Claude Desktop session lineage binding is invalid")
	}
	return record, files, nil
}

func (w *accountWorker) writeLineage(record lineageRecord) (string, error) {
	payload, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal Claude Desktop session lineage: %w", errMarshal)
	}
	digest := sha256String(record.SessionID)
	path := filepath.Join(w.sessionDir, fmt.Sprintf("%s_%020d.lineage", digest, record.Revision))
	if errWrite := writeProtectedFile(path, payload); errWrite != nil {
		return "", fmt.Errorf("persist Claude Desktop session lineage: %w", errWrite)
	}
	return path, nil
}

func removeLineageSnapshots(paths []string, keep string) error {
	var joined error
	for _, path := range paths {
		if path == keep {
			continue
		}
		if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			joined = errors.Join(joined, errRemove)
		}
	}
	return joined
}

func (w *accountWorker) enqueueProjected(ctx context.Context, fact, sessionID, clientRequestID string, metadata map[string]any) error {
	_ = ctx
	envelope, errProject := w.manager.project(w.binding, fact, sessionID, clientRequestID, metadata)
	if errProject != nil {
		return errProject
	}
	return w.enqueue(envelope)
}

func (w *accountWorker) enqueue(envelope Envelope) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.manager.freezeOnShutdown.Load() {
		return context.Canceled
	}
	files, errScan := w.scanQueue()
	if errScan != nil {
		w.status.queueWritable = false
		return errScan
	}
	pending := 0
	for _, file := range files {
		if file.state == "pending" || strings.HasPrefix(file.state, "sending_") {
			pending++
		}
	}
	if pending >= w.profile.batch.MaxPendingEvents {
		w.status.queueWritable = false
		return fmt.Errorf("telemetry queue reached max_pending_events=%d", w.profile.batch.MaxPendingEvents)
	}
	w.nextSequence++
	envelope.Sequence = w.nextSequence
	payload, errMarshal := json.Marshal(envelope)
	if envelope.CatalogFact == factGrowthbookExposure {
		payload, errMarshal = marshalGrowthbookQueueEnvelope(envelope)
	}
	if errMarshal != nil {
		return errMarshal
	}
	name := pendingQueueName(envelope.Sequence, envelope.EventUUID, 0, w.manager.now())
	if errWrite := writeProtectedFile(filepath.Join(w.directory, name), payload); errWrite != nil {
		w.status.queueWritable = false
		return errWrite
	}
	w.status.queueWritable = true
	if pending+1 >= w.profile.batch.MaxEvents {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (w *accountWorker) run(ctx context.Context) {
	timer := time.NewTimer(w.nextFlushDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if w.manager == nil || !w.manager.freezeOnShutdown.Load() {
				_ = w.flush(context.Background())
			}
			return
		case <-w.wake:
			_ = w.flush(ctx)
		case <-timer.C:
			_ = w.flush(ctx)
		}
		timer.Reset(w.nextFlushDelay())
	}
}

func (w *accountWorker) nextFlushDelay() time.Duration {
	batch := w.profile.batch
	factor := batch.JitterMinimum
	if batch.JitterMaximum > batch.JitterMinimum {
		factor += w.manager.randomFloat() * (batch.JitterMaximum - batch.JitterMinimum)
	}
	return time.Duration(float64(time.Duration(batch.FlushIntervalMS)*time.Millisecond) * factor)
}

func (w *accountWorker) flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Explicit management flushes and graceful final flushes share the same
	// quarantine cancellation owner, without installing a network deadline.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.manager.shutdownCtx, cancel)
	defer func() { stop(); cancel() }()
	if w.manager.shutdownCtx.Err() != nil {
		cancel()
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := w.manager.now()
	if errRecover := w.recoverStaleClaims(now); errRecover != nil {
		w.recordQueueFailure(errRecover)
		w.recordDeliveryFailure(errRecover, time.Time{})
		return errRecover
	}
	hasReady, errReady := w.hasReadyEvent(now)
	if errReady != nil {
		w.recordQueueFailure(errReady)
		w.recordDeliveryFailure(errReady, time.Time{})
		return errReady
	}
	if !hasReady {
		return nil
	}
	authorization, errAuthorization := w.authorizationHeader()
	if errAuthorization != nil {
		w.recordDeliveryFailure(errAuthorization, time.Time{})
		return errAuthorization
	}
	materials, errMaterials := runtimeMaterialsForDelivery(w.authSnapshot(), w.profile)
	if errMaterials != nil {
		w.recordDeliveryFailure(errMaterials, time.Time{})
		return errMaterials
	}
	claimed, envelopes, errClaim := w.claimBatch(now)
	if errClaim != nil {
		w.recordQueueFailure(errClaim)
		w.recordDeliveryFailure(errClaim, time.Time{})
		return errClaim
	}
	if len(claimed) == 0 {
		return nil
	}
	encodedBatch, errMarshal := encodeDeliveryBatch(w.profile, envelopes, materials, w.manager.now())
	if errMarshal != nil {
		return errors.Join(errMarshal, w.releaseClaims(claimed, errMarshal))
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, w.profile.endpoint, bytes.NewReader(encodedBatch.body))
	if errRequest != nil {
		return errors.Join(errRequest, w.releaseClaims(claimed, errRequest))
	}
	for _, header := range w.profile.headers {
		request.Header.Set(header.Name, header.Value)
	}
	if w.profile.rendererRuntime != nil || w.profile.sentry != nil {
		if errRenderer := applyRendererRequestHeaders(request, w.profile.rendererRuntime, w.profile.sentry, w.manager.hostSnapshot); errRenderer != nil {
			return errors.Join(errRenderer, w.releaseClaims(claimed, errRenderer))
		}
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if errAuxiliary := applyAuxiliaryRequest(request, w.profile, materials, encodedBatch); errAuxiliary != nil {
		return errors.Join(errAuxiliary, w.releaseClaims(claimed, errAuxiliary))
	}
	if w.profile.userAgentPolicy == "omit" {
		request.Header["User-Agent"] = nil
	}
	response, errDo := w.currentDoer().Do(request)
	if errDo != nil {
		return errors.Join(errDo, w.releaseClaims(claimed, errDo))
	}
	if response == nil {
		errResponse := errors.New("telemetry endpoint returned no response")
		return errors.Join(errResponse, w.releaseClaims(claimed, errResponse))
	}
	wantProtoMajor := 0
	switch w.profile.protocol {
	case "http/1.1":
		wantProtoMajor = 1
	case "http/2":
		wantProtoMajor = 2
	}
	if wantProtoMajor != 0 && response.ProtoMajor != wantProtoMajor {
		errProtocol := fmt.Errorf("telemetry endpoint negotiated %q instead of %s", response.Proto, w.profile.protocol)
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return errors.Join(errProtocol, w.releaseClaims(claimed, errProtocol))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	errClose := response.Body.Close()
	if err := ctx.Err(); err != nil {
		return errors.Join(err, w.releaseClaims(claimed, err))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		errStatus := fmt.Errorf("telemetry endpoint returned HTTP %d", response.StatusCode)
		return errors.Join(errStatus, w.releaseClaims(claimed, errStatus))
	}
	if errClose != nil {
		return errors.Join(errClose, w.releaseClaims(claimed, errClose))
	}
	for _, file := range claimed {
		if errRemove := os.Remove(file.path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			w.recordQueueFailure(errRemove)
			w.recordDeliveryFailure(errRemove, time.Time{})
			return errRemove
		}
	}
	w.mu.Lock()
	w.status.consecutiveFailures = 0
	w.status.lastSuccessAt = w.manager.now().UTC()
	w.status.lastError = ""
	w.status.nextAttemptAt = time.Time{}
	w.status.queueWritable = true
	w.mu.Unlock()
	return nil
}

func (w *accountWorker) authorizationHeader() (string, error) {
	if w == nil || w.profile.authPolicy == "" || w.profile.authPolicy == "runtime-material" {
		return "", nil
	}
	if w.profile.authPolicy != "oauth-bearer" {
		return "", fmt.Errorf("telemetry authentication policy is unsupported")
	}
	w.mu.Lock()
	auth := w.auth
	w.mu.Unlock()
	if auth == nil {
		return "", fmt.Errorf("telemetry OAuth credential is unavailable")
	}
	// The worker already validates Desktop enrollment. Use the same current
	// credential precedence as inference/startup, including account-pool OAuth
	// snapshots whose token is carried in attributes rather than metadata.
	accessToken := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeAPIKey])
	if accessToken == "" {
		accessToken, _ = auth.Metadata["access_token"].(string)
		accessToken = strings.TrimSpace(accessToken)
	}
	if accessToken == "" {
		return "", fmt.Errorf("telemetry OAuth credential is unavailable")
	}
	return "Bearer " + accessToken, nil
}

func (w *accountWorker) hasReadyEvent(now time.Time) (bool, error) {
	files, errScan := w.scanQueue()
	if errScan != nil {
		return false, errScan
	}
	for _, file := range files {
		if file.state == "pending" && !file.nextAttemptAt.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func (w *accountWorker) claimBatch(now time.Time) ([]queueFile, []Envelope, error) {
	files, errScan := w.scanQueue()
	if errScan != nil {
		return nil, nil, errScan
	}
	sort.Slice(files, func(i, j int) bool { return files[i].sequence < files[j].sequence })
	claimed := make([]queueFile, 0, w.profile.batch.MaxEvents)
	envelopes := make([]Envelope, 0, w.profile.batch.MaxEvents)
	bodyBytes := len(`{"events":[]}`)
	leaseUntil := now.Add(time.Duration(w.profile.batch.LeaseTimeoutMS) * time.Millisecond)
	for _, file := range files {
		if file.state != "pending" || file.nextAttemptAt.After(now) {
			continue
		}
		claimName := strings.TrimSuffix(file.name, ".pending") + fmt.Sprintf(".sending_%d_%s", leaseUntil.UnixMilli(), w.workerID)
		claimPath := filepath.Join(w.directory, claimName)
		if errRename := os.Rename(file.path, claimPath); errRename != nil {
			if errors.Is(errRename, os.ErrNotExist) {
				continue
			}
			return claimed, envelopes, errRename
		}
		file.path = claimPath
		file.name = claimName
		file.state = "sending"
		payload, errRead := readProtectedFile(claimPath)
		if errRead != nil {
			if errDead := w.moveToDead(file); errDead != nil {
				return claimed, envelopes, errors.Join(errRead, errDead)
			}
			continue
		}
		var envelope Envelope
		if errUnmarshal := json.Unmarshal(payload, &envelope); errUnmarshal != nil || envelope.Version != 1 || envelope.Binding.BindingRevision != w.binding.BindingRevision {
			if errDead := w.moveToDead(file); errDead != nil {
				return claimed, envelopes, errors.Join(errUnmarshal, errDead)
			}
			continue
		}
		eventBytes := envelopePayloadSize(envelope)
		if bodyBytes+eventBytes+1 > w.profile.batch.MaxBytes {
			if len(claimed) > 0 {
				if errReturn := w.returnClaim(file, file.attempt, now); errReturn != nil {
					return claimed, envelopes, errReturn
				}
				break
			}
			if errDead := w.moveToDead(file); errDead != nil {
				return claimed, envelopes, errDead
			}
			continue
		}
		if len(claimed) >= w.profile.batch.MaxEvents {
			if errReturn := w.returnClaim(file, file.attempt, now); errReturn != nil {
				return claimed, envelopes, errReturn
			}
			break
		}
		bodyBytes += eventBytes + 1
		claimed = append(claimed, file)
		envelopes = append(envelopes, envelope)
		if len(claimed) >= w.profile.batch.MaxEvents {
			break
		}
	}
	return claimed, envelopes, nil
}

func (w *accountWorker) releaseClaims(files []queueFile, cause error) error {
	now := w.manager.now()
	var next time.Time
	var joined error
	for _, file := range files {
		attempt := file.attempt + 1
		if attempt > w.profile.batch.MaxRetries {
			file.attempt = attempt
			joined = errors.Join(joined, w.moveToDead(file))
			continue
		}
		delay := w.retryDelay(attempt)
		nextAttempt := now.Add(delay)
		if next.IsZero() || nextAttempt.Before(next) {
			next = nextAttempt
		}
		joined = errors.Join(joined, w.returnClaim(file, attempt, nextAttempt))
	}
	w.recordDeliveryFailure(errors.Join(cause, joined), next)
	if joined != nil {
		w.recordQueueFailure(joined)
	}
	return joined
}

func (w *accountWorker) retryDelay(attempt int) time.Duration {
	batch := w.profile.batch
	delay := time.Duration(batch.InitialBackoffMS) * time.Millisecond
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= time.Duration(batch.MaxBackoffMS)*time.Millisecond {
			delay = time.Duration(batch.MaxBackoffMS) * time.Millisecond
			break
		}
	}
	factor := batch.JitterMinimum
	if batch.JitterMaximum > batch.JitterMinimum {
		factor += w.manager.randomFloat() * (batch.JitterMaximum - batch.JitterMinimum)
	}
	return time.Duration(float64(delay) * factor)
}

func (w *accountWorker) returnClaim(file queueFile, attempt int, next time.Time) error {
	name := pendingQueueName(file.sequence, file.eventUUID, attempt, next)
	return os.Rename(file.path, filepath.Join(w.directory, name))
}

func (w *accountWorker) moveToDead(file queueFile) error {
	name := deadQueueName(file.sequence, file.eventUUID, file.attempt, w.manager.now())
	if errRename := os.Rename(file.path, filepath.Join(w.directory, name)); errRename != nil {
		return errRename
	}
	return w.enforceDeadLetterLimit()
}

func (w *accountWorker) enforceDeadLetterLimit() error {
	maximum := w.profile.batch.MaxDeadLetters
	files, errScan := w.scanQueue()
	if errScan != nil {
		return errScan
	}
	dead := make([]queueFile, 0, len(files))
	for _, file := range files {
		if file.state == "dead" {
			dead = append(dead, file)
		}
	}
	if len(dead) <= maximum {
		return nil
	}
	sort.Slice(dead, func(i, j int) bool { return dead[i].sequence < dead[j].sequence })
	var joined error
	for _, file := range dead[:len(dead)-maximum] {
		if errRemove := os.Remove(file.path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			joined = errors.Join(joined, errRemove)
		}
	}
	return joined
}

func (w *accountWorker) recoverStaleClaims(now time.Time) error {
	files, errScan := w.scanQueue()
	if errScan != nil {
		return errScan
	}
	for _, file := range files {
		if !strings.HasPrefix(file.state, "sending_") {
			continue
		}
		parts := strings.Split(file.state, "_")
		if len(parts) < 3 {
			continue
		}
		leaseMS, errLease := strconv.ParseInt(parts[1], 10, 64)
		if errLease != nil || now.UnixMilli() < leaseMS {
			continue
		}
		if errReturn := w.returnClaim(file, file.attempt, now); errReturn != nil && !errors.Is(errReturn, os.ErrNotExist) {
			return errReturn
		}
	}
	return nil
}

func (w *accountWorker) retryDeadLetters() (int, error) {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	files, errScan := w.scanQueue()
	if errScan != nil {
		w.recordQueueFailure(errScan)
		return 0, errScan
	}
	count := 0
	var joined error
	now := w.manager.now()
	for _, file := range files {
		if file.state != "dead" {
			continue
		}
		if errRename := os.Rename(file.path, filepath.Join(w.directory, pendingQueueName(file.sequence, file.eventUUID, 0, now))); errRename != nil {
			joined = errors.Join(joined, errRename)
			continue
		}
		count++
	}
	if count > 0 {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	if joined != nil {
		w.recordQueueFailure(joined)
	}
	return count, joined
}

func (w *accountWorker) statusSnapshot() AccountStatus {
	files, errScan := w.scanQueue()
	status := AccountStatus{
		AuthIDHash:     authIDHash(w.binding),
		ProfileID:      w.binding.ProfileID,
		DesktopVersion: w.binding.DesktopVersion,
		EndpointRole:   w.profile.endpointRole,
		Health:         "healthy",
	}
	for _, file := range files {
		switch {
		case file.state == "pending":
			status.Pending++
			if status.NextAttemptAt == nil || file.nextAttemptAt.Before(*status.NextAttemptAt) {
				status.NextAttemptAt = timePointer(file.nextAttemptAt)
			}
		case file.state == "dead":
			status.DeadLetters++
		case strings.HasPrefix(file.state, "sending_"):
			status.Sending++
		}
	}
	w.mu.Lock()
	runtimeStatus := w.status
	w.mu.Unlock()
	status.ConsecutiveFailures = runtimeStatus.consecutiveFailures
	status.LastSuccessAt = timePointer(runtimeStatus.lastSuccessAt)
	status.LastFailureAt = timePointer(runtimeStatus.lastFailureAt)
	status.LastError = runtimeStatus.lastError
	status.QueueWritable = runtimeStatus.queueWritable && errScan == nil
	if !runtimeStatus.nextAttemptAt.IsZero() && (status.NextAttemptAt == nil || runtimeStatus.nextAttemptAt.Before(*status.NextAttemptAt)) {
		status.NextAttemptAt = timePointer(runtimeStatus.nextAttemptAt)
	}
	if errScan != nil {
		status.Health = "unhealthy"
		status.LastError = safeStatusError(errScan)
	} else if !status.QueueWritable {
		status.Health = "unhealthy"
	} else if status.DeadLetters > 0 || status.ConsecutiveFailures > 0 {
		status.Health = "degraded"
	}
	status.FactIssues = w.factIssueSnapshot()
	if status.FactIssues != nil && status.Health == "healthy" {
		status.Health = "degraded"
	}
	return status
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func (w *accountWorker) recordQueueFailure(err error) {
	w.mu.Lock()
	w.status.queueWritable = false
	w.status.lastFailureAt = w.manager.now().UTC()
	w.status.lastError = safeStatusError(err)
	w.mu.Unlock()
}

func (w *accountWorker) recordDeliveryFailure(err error, next time.Time) {
	w.mu.Lock()
	w.status.consecutiveFailures++
	w.status.lastFailureAt = w.manager.now().UTC()
	w.status.lastError = safeStatusError(err)
	w.status.nextAttemptAt = next
	w.mu.Unlock()
}

func (w *accountWorker) scanQueue() ([]queueFile, error) {
	entries, errRead := os.ReadDir(w.directory)
	if errRead != nil {
		return nil, errRead
	}
	files := make([]queueFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := queueFilePattern.FindStringSubmatch(entry.Name())
		if len(match) != 6 {
			continue
		}
		sequence, errSequence := strconv.ParseUint(match[1], 10, 64)
		attempt, errAttempt := strconv.Atoi(match[3])
		nextMS, errNext := strconv.ParseInt(match[4], 10, 64)
		if errSequence != nil || errAttempt != nil || errNext != nil {
			continue
		}
		files = append(files, queueFile{
			path:          filepath.Join(w.directory, entry.Name()),
			name:          entry.Name(),
			sequence:      sequence,
			eventUUID:     match[2],
			attempt:       attempt,
			nextAttemptAt: time.UnixMilli(nextMS),
			state:         match[5],
		})
	}
	return files, nil
}

func pendingQueueName(sequence uint64, eventUUID string, attempt int, next time.Time) string {
	return fmt.Sprintf("%020d_%s_a%d_t%d.pending", sequence, eventUUID, attempt, next.UnixMilli())
}

func deadQueueName(sequence uint64, eventUUID string, attempt int, at time.Time) string {
	return fmt.Sprintf("%020d_%s_a%d_t%d.dead", sequence, eventUUID, attempt, at.UnixMilli())
}

func writeProtectedFile(path string, plaintext []byte) error {
	protector, ciphertext, errProtect := claudedesktop.ProtectRuntimePayload(path, plaintext)
	if errProtect != nil {
		return fmt.Errorf("protect telemetry queue item: %w", errProtect)
	}
	record, errMarshal := json.Marshal(protectedRecord{
		Version:    protectedRecordVersion,
		Protector:  protector,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if errMarshal != nil {
		return errMarshal
	}
	temporary := filepath.Join(filepath.Dir(path), ".tmp-"+randomID())
	file, errOpen := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		return errOpen
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, errWrite := file.Write(record); errWrite != nil {
		_ = file.Close()
		return errWrite
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return errSync
	}
	if errClose := file.Close(); errClose != nil {
		return errClose
	}
	if errRename := os.Rename(temporary, path); errRename != nil {
		return errRename
	}
	removeTemporary = false
	return nil
}

func readProtectedFile(path string) ([]byte, error) {
	encoded, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	var record protectedRecord
	if errUnmarshal := json.Unmarshal(encoded, &record); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if record.Version != protectedRecordVersion {
		return nil, fmt.Errorf("unsupported protected telemetry record version %d", record.Version)
	}
	ciphertext, errDecode := base64.StdEncoding.DecodeString(record.Ciphertext)
	if errDecode != nil {
		return nil, errDecode
	}
	return claudedesktop.UnprotectRuntimePayload(path, record.Protector, ciphertext)
}

func safeStatusError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	default:
		if status, ok := err.(interface{ StatusCode() int }); ok {
			code := status.StatusCode()
			if code > 0 {
				return fmt.Sprintf("http_status_%d", code)
			}
		}
		return "delivery_error"
	}
}

func sha256String(value string) string {
	return fmt.Sprintf("%x", sha256Bytes([]byte(value)))
}

func sha256Bytes(value []byte) [32]byte {
	return sha256.Sum256(value)
}
