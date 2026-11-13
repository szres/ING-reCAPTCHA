package miniapp

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/szres/ing-recaptcha/internal/database"
)

//go:embed static
var staticFS embed.FS

// Deps is the set of bot-side capabilities the Mini App server needs. It's
// passed in as closures (rather than typed interfaces) so this package can
// stay free of `internal/bot` and avoid an import cycle.
type Deps struct {
	// DB is the shared SQLite handle. The server reads + transitions
	// miniapp_sessions rows on it.
	DB *database.DB
	// BotToken is the long-poll token used to verify Telegram WebApp initData
	// HMAC. It must be the SAME token Telegram used to sign initData.
	BotToken string
	// SessionTTL bounds how long after creation a nonce can still be used.
	// Codex's review pushed this from 90s to 300s (5 min) to accommodate slow
	// mobile networks and accessibility-paced solvers.
	SessionTTL time.Duration
	// RequiredCorrect is the minimum number of correct picks needed to pass.
	// Must be <= len(correctLabels) returned by ComposeChallenge. Mirrors the
	// in-group flow's VERIFY_REQUIRED_CORRECT so the captcha difficulty is
	// the same whether the user goes through the Mini App or the in-group
	// keyboard.
	RequiredCorrect int
	// ComposeChallenge builds one verification image and returns its temp
	// path, the ordered correct labels (one per sub-image), and the shuffled
	// label keypad to present. Cleanup of composedPath is the server's
	// responsibility once the challenge state is dropped.
	ComposeChallenge func() (composedPath string, correctLabels []string, allOptions []string, err error)
	// CleanupTempFile removes a composed-image temp file. Mirrors the
	// Composer.CleanupTempFile method but doesn't tie this package to the
	// imaging Composer type.
	CleanupTempFile func(path string)
	// ApproveJoinRequest calls answerChatJoinRequestQuery(queryID, "approve").
	// Must complete within Telegram's 10-second window for the query_id.
	// Used for the modern Mini App flow where the session carries a query_id.
	ApproveJoinRequest func(ctx context.Context, queryID string) error
	// DeclineJoinRequest calls answerChatJoinRequestQuery(queryID, "decline").
	DeclineJoinRequest func(ctx context.Context, queryID string) error
	// ApproveJoinLegacy calls approveChatJoinRequest(chat_id, user_id), the
	// pre-Guard-Bots admit API. Used for the DM-fallback flow where the
	// session has no query_id — the user solved the Mini App in DM after a
	// chat_join_request that Telegram delivered without query_id, and we
	// need to admit them via the legacy path.
	ApproveJoinLegacy func(ctx context.Context, chatID, userID int64) error
	// DeclineJoinLegacy calls declineChatJoinRequest(chat_id, user_id).
	// Used for the DM-fallback flow on captcha failure.
	DeclineJoinLegacy func(ctx context.Context, chatID, userID int64) error
	// OnApproved fires after a successful approve on the join-request side,
	// from the same goroutine. The bot uses this to send the optional silent
	// welcome message in the group. Synchronous so the HTTP response can
	// reflect the welcome's failure state if needed.
	OnApproved func(chatID, userID int64)
	// OnDeclined fires after a successful decline on the join-request side.
	// The bot uses this to notify configured watchers (FAIL_NOTIFY_*) that
	// a verification failed in a monitored chat. Called synchronously —
	// keep the implementation short so it doesn't hold up the HTTP
	// response to the Mini App client. reason is a short tag like
	// "captcha-failed" so the notifier can differentiate paths.
	OnDeclined func(chatID, userID int64, reason string)
}

// Server is the HTTP server that backs the Mini App. One per bot process.
type Server struct {
	deps       Deps
	mux        *http.ServeMux
	httpSrv    *http.Server
	challenges sync.Map // nonce → *challengeState
}

type challengeState struct {
	mu sync.Mutex

	chatID  int64
	userID  int64
	queryID string

	composedPath  string
	correctLabels []string
	allOptions    []string
	currentStep   int
	// requiredCorrect mirrors VerifyRequiredCorrect — the threshold compared
	// against correctCount once all steps are answered. Stored per-challenge
	// so reconfiguration mid-challenge can't move the goalposts.
	requiredCorrect int
	correctCount    int

	expiresAt time.Time
	done      bool
}

// New constructs a Server with the given listen addr and dependencies.
// Call Start to begin serving and Stop to gracefully shut down.
func New(listenAddr string, deps Deps) *Server {
	s := &Server{deps: deps}
	s.mux = http.NewServeMux()
	s.routes()
	s.httpSrv = &http.Server{
		Addr:              listenAddr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10, // 16 KiB; initData is well under
	}
	return s
}

// Start blocks until the server stops. Run in a goroutine.
func (s *Server) Start() error {
	log.Printf("[INFO] Mini App server listening on %s", s.httpSrv.Addr)
	err := s.httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop attempts a graceful shutdown.
func (s *Server) Stop(ctx context.Context) error {
	s.cleanupAllChallenges()
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v", s.handleIndex)
	s.mux.HandleFunc("/v/", s.handleStaticOrAPI)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// handleIndex serves the Mini App's HTML shell. Validation happens on the
// API endpoints — the HTML is generic, so an unauthenticated GET only
// exposes the static page (no per-session data).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.serveStatic(w, r, "index.html", "text/html; charset=utf-8")
}

// handleStaticOrAPI dispatches /v/* requests to either static-asset or API
// handlers. Keeps the route table small.
func (s *Server) handleStaticOrAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v/init" && r.Method == http.MethodPost:
		s.handleInit(w, r)
	case r.URL.Path == "/v/answer" && r.Method == http.MethodPost:
		s.handleAnswer(w, r)
	case strings.HasPrefix(r.URL.Path, "/v/image/") && r.Method == http.MethodGet:
		s.handleImage(w, r)
	case strings.HasPrefix(r.URL.Path, "/v/static/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(r.URL.Path, "/v/static/")
		ct := guessContentType(name)
		s.serveStatic(w, r, name, ct)
	default:
		http.NotFound(w, r)
	}
}

// initRequest is the body the Mini App posts on load.
type initRequest struct {
	Nonce    string `json:"nonce"`
	InitData string `json:"init_data"`
}

// initResponse is what the Mini App receives after successful init.
type initResponse struct {
	ImageURL     string   `json:"image_url"`
	Options      []string `json:"options"`
	TotalSteps   int      `json:"total_steps"`
	CurrentStep  int      `json:"current_step"`
	ExpiresAtSec int64    `json:"expires_at_sec"`
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	sess, err := s.authorize(req.Nonce, req.InitData)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	state, fresh, err := s.loadOrBuildChallenge(sess)
	if err != nil {
		log.Printf("[ERROR] miniapp init: compose challenge for nonce=%s: %v", sess.Nonce, err)
		writeJSONError(w, http.StatusInternalServerError, "compose failed")
		return
	}
	_ = fresh

	state.mu.Lock()
	resp := initResponse{
		ImageURL:     "/v/image/" + sess.Nonce,
		Options:      append([]string(nil), state.allOptions...),
		TotalSteps:   len(state.correctLabels),
		CurrentStep:  state.currentStep,
		ExpiresAtSec: sess.ExpiresAt,
	}
	state.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// answerRequest is the body posted on each label tap.
type answerRequest struct {
	Nonce    string `json:"nonce"`
	InitData string `json:"init_data"`
	Step     int    `json:"step"`
	Label    string `json:"label"`
}

// answerResponse describes what the frontend should do next.
type answerResponse struct {
	// Status is one of "next", "passed", "failed".
	Status      string `json:"status"`
	CurrentStep int    `json:"current_step"`
	TotalSteps  int    `json:"total_steps"`
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var req answerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	sess, err := s.authorize(req.Nonce, req.InitData)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	value, ok := s.challenges.Load(sess.Nonce)
	if !ok {
		// No active challenge — caller skipped /v/init or it was cleaned up.
		writeJSONError(w, http.StatusGone, "no active challenge")
		return
	}
	state := value.(*challengeState)

	state.mu.Lock()
	if state.done {
		state.mu.Unlock()
		writeJSONError(w, http.StatusGone, "challenge already finished")
		return
	}
	if req.Step != state.currentStep {
		// Out-of-order submission — frontend bug or replay.
		state.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "out of order")
		return
	}

	// Mirror the in-group flow: every step is recorded (right or wrong),
	// the challenge runs to completion, and we evaluate
	// correctCount >= requiredCorrect at the end. This keeps Mini App and
	// in-group captcha rules identical — required=3 of 4 means the user
	// can miss one without failing on the spot.
	expected := state.correctLabels[state.currentStep]
	if req.Label == expected {
		state.correctCount++
	}
	state.currentStep++
	currentStep := state.currentStep
	totalSteps := len(state.correctLabels)
	correctCount := state.correctCount
	required := state.requiredCorrect

	// Early-fail: even if every remaining answer is correct we can't reach
	// the threshold. Cuts UX where the user keeps tapping after the result
	// is already decided.
	remaining := totalSteps - currentStep
	maxAchievable := correctCount + remaining
	if maxAchievable < required {
		state.done = true
		state.mu.Unlock()
		s.finishAndDecline(sess, state)
		writeJSON(w, http.StatusOK, answerResponse{
			Status: "failed", CurrentStep: currentStep, TotalSteps: totalSteps,
		})
		return
	}

	if currentStep < totalSteps {
		state.mu.Unlock()
		writeJSON(w, http.StatusOK, answerResponse{
			Status: "next", CurrentStep: currentStep, TotalSteps: totalSteps,
		})
		return
	}

	// All steps answered — evaluate.
	state.done = true
	state.mu.Unlock()
	if correctCount >= required {
		s.finishAndApprove(sess, state)
		writeJSON(w, http.StatusOK, answerResponse{
			Status: "passed", CurrentStep: currentStep, TotalSteps: totalSteps,
		})
		return
	}
	s.finishAndDecline(sess, state)
	writeJSON(w, http.StatusOK, answerResponse{
		Status: "failed", CurrentStep: currentStep, TotalSteps: totalSteps,
	})
}

// handleImage serves the composed PNG for an in-flight challenge. The
// nonce is the only handle; it's single-use and high-entropy, so the image
// itself is treated as a capability granted by the nonce.
//
// We do NOT require initData on the image fetch because the Telegram WebApp
// fetches it via a plain <img> tag where injecting custom headers is
// impossible. The nonce is single-use upstream (the user can only get one
// per join request, bound to a single user), so this is the same security
// posture as the in-group flow where the PNG is just a chat attachment.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	nonce := strings.TrimPrefix(r.URL.Path, "/v/image/")
	if nonce == "" {
		http.NotFound(w, r)
		return
	}
	value, ok := s.challenges.Load(nonce)
	if !ok {
		http.NotFound(w, r)
		return
	}
	state := value.(*challengeState)
	state.mu.Lock()
	path := state.composedPath
	done := state.done
	state.mu.Unlock()
	if done || path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[ERROR] miniapp image open: %v", err)
		http.Error(w, "image unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filepath.Base(path), time.Now(), f)
}

// authorize validates initData against bot token, checks freshness, looks
// up the nonce, and confirms the initData user_id matches the user bound
// to the nonce. Returns the live DB row on success.
func (s *Server) authorize(nonce, initData string) (*database.MiniAppSession, error) {
	if nonce == "" {
		return nil, ErrMissingInitData
	}
	uid, _, err := ValidateInitData(initData, s.deps.BotToken, time.Now())
	if err != nil {
		return nil, err
	}
	sess, err := s.deps.DB.GetMiniAppSession(nonce)
	if err != nil {
		return nil, errUnknownNonce
	}
	if sess.Status != "pending" {
		return nil, errSessionClosed
	}
	if sess.ExpiresAt <= time.Now().Unix() {
		return nil, errSessionClosed
	}
	if sess.UserID != uid {
		// User binding: the Telegram-signed initData claims a user_id that
		// doesn't match the user who issued the join request. Either a
		// proxy attack or a race; in either case, refuse.
		return nil, errUserMismatch
	}
	return sess, nil
}

// loadOrBuildChallenge returns the in-memory challenge state for a session,
// composing a new one if this is the first /v/init call.
func (s *Server) loadOrBuildChallenge(sess *database.MiniAppSession) (*challengeState, bool, error) {
	if v, ok := s.challenges.Load(sess.Nonce); ok {
		return v.(*challengeState), false, nil
	}
	composedPath, correctLabels, allOptions, err := s.deps.ComposeChallenge()
	if err != nil {
		return nil, false, err
	}
	required := s.deps.RequiredCorrect
	if required <= 0 || required > len(correctLabels) {
		// Defensive fallback: if config is missing or nonsensical, demand
		// every answer be correct. This mirrors a "strict" captcha and
		// keeps the server from accidentally accepting a wide-open pass.
		required = len(correctLabels)
	}
	state := &challengeState{
		chatID:          sess.ChatID,
		userID:          sess.UserID,
		queryID:         sess.QueryID,
		composedPath:    composedPath,
		correctLabels:   correctLabels,
		allOptions:      allOptions,
		currentStep:     0,
		requiredCorrect: required,
		expiresAt:       time.Unix(sess.ExpiresAt, 0),
	}
	actual, loaded := s.challenges.LoadOrStore(sess.Nonce, state)
	if loaded {
		// Race: someone else built it first. Clean up our temp file and
		// return theirs.
		s.deps.CleanupTempFile(composedPath)
		return actual.(*challengeState), false, nil
	}
	return state, true, nil
}

// finishAndApprove reserves the session via a transient 'approving' status,
// calls Telegram's answerChatJoinRequestQuery(approve), and only on
// confirmed success transitions to 'approved_prejoin' and fires OnApproved.
//
// Order matters because of a chat_member race: when Telegram approves the
// user, it fires a chat_member 'left → member' update almost instantly —
// often arriving on our long-poll BEFORE the post-approve DB write commits.
// The chat_member handler queries HasRecentMiniAppApproval to skip the
// in-group captcha for users we just admitted via Mini App. If the DB
// hasn't been marked yet when that query runs, the guard misses and we
// start a redundant in-group verification on top of the one the user just
// solved.
//
// Stamping 'approving' BEFORE the Telegram call closes the window:
// HasRecentMiniAppApproval matches both 'approving' and 'approved_prejoin',
// so chat_member can arrive at any point after the API call returns and
// still be caught.
//
// On API failure we transition 'approving → expired' to free the
// unique-pending partial-index slot for retries. (Telegram never admitted
// the user in that case, so there's no chat_member to race against.)
//
// Telegram's 10-second window for query_id makes this latency-sensitive,
// so the approve call runs with a short context (8s) just under the
// window.
func (s *Server) finishAndApprove(sess *database.MiniAppSession, state *challengeState) {
	defer s.dropChallenge(sess.Nonce)

	if ok, err := s.deps.DB.MarkMiniAppSession(sess.Nonce, "approving"); err != nil {
		log.Printf("[ERROR] reserve approving for nonce=%s: %v", sess.Nonce, err)
		return
	} else if !ok {
		log.Printf("[WARN] mini app session %s no longer pending — skipping approve", sess.Nonce)
		return
	}

	// The captcha is solved, so the user goes on the global pass list now
	// rather than after the approve call. If Telegram rejects the approval
	// (typically an expired query_id), their next join request is admitted
	// from the pass list instead of making them solve a second captcha.
	if err := s.deps.DB.MarkUserVerified(sess.UserID, sess.ChatID, database.VerifiedSourceMiniApp); err != nil {
		log.Printf("[ERROR] add user %d to the pass list for nonce=%s: %v", sess.UserID, sess.Nonce, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Branch on whether the session carries a query_id. With one, we use
	// the new query-bound approve method. Without one (DM-fallback path),
	// we fall back to legacy approveChatJoinRequest(chat_id, user_id).
	var approveErr error
	if sess.QueryID != "" {
		approveErr = s.deps.ApproveJoinRequest(ctx, sess.QueryID)
	} else if s.deps.ApproveJoinLegacy != nil {
		approveErr = s.deps.ApproveJoinLegacy(ctx, sess.ChatID, sess.UserID)
	} else {
		approveErr = fmt.Errorf("no approve handler available for legacy session nonce=%s", sess.Nonce)
	}
	if approveErr != nil {
		log.Printf("[ERROR] approve join request for nonce=%s: %v", sess.Nonce, approveErr)
		// Revert the reservation so the unique-active slot frees up.
		if _, mErr := s.deps.DB.MarkMiniAppSession(sess.Nonce, "expired"); mErr != nil {
			log.Printf("[ERROR] mark session expired after approve failure: %v", mErr)
		}
		return
	}

	if ok, err := s.deps.DB.MarkMiniAppSession(sess.Nonce, "approved_prejoin"); err != nil {
		log.Printf("[ERROR] mark approved_prejoin after Telegram approve succeeded for nonce=%s: %v", sess.Nonce, err)
	} else if !ok {
		log.Printf("[WARN] mini app session %s no longer approving after Telegram approve — concurrent state change?", sess.Nonce)
	}

	if s.deps.OnApproved != nil {
		s.deps.OnApproved(state.chatID, state.userID)
	}
}

// finishAndDecline rejects the join request via the appropriate API and
// only on confirmed success marks the session declined. Branches on
// query_id presence the same way finishAndApprove does.
func (s *Server) finishAndDecline(sess *database.MiniAppSession, _ *challengeState) {
	defer s.dropChallenge(sess.Nonce)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var declineErr error
	if sess.QueryID != "" {
		declineErr = s.deps.DeclineJoinRequest(ctx, sess.QueryID)
	} else if s.deps.DeclineJoinLegacy != nil {
		declineErr = s.deps.DeclineJoinLegacy(ctx, sess.ChatID, sess.UserID)
	} else {
		declineErr = fmt.Errorf("no decline handler available for legacy session nonce=%s", sess.Nonce)
	}
	if declineErr != nil {
		log.Printf("[ERROR] decline join request for nonce=%s: %v", sess.Nonce, declineErr)
		return
	}
	if _, err := s.deps.DB.MarkMiniAppSession(sess.Nonce, "declined"); err != nil {
		log.Printf("[ERROR] mark declined after Telegram decline succeeded for nonce=%s: %v", sess.Nonce, err)
	}
	if s.deps.OnDeclined != nil {
		s.deps.OnDeclined(sess.ChatID, sess.UserID, "captcha-failed")
	}
}

func (s *Server) dropChallenge(nonce string) {
	if v, ok := s.challenges.LoadAndDelete(nonce); ok {
		st := v.(*challengeState)
		if st.composedPath != "" {
			s.deps.CleanupTempFile(st.composedPath)
		}
	}
}

func (s *Server) cleanupAllChallenges() {
	s.challenges.Range(func(k, v interface{}) bool {
		st := v.(*challengeState)
		if st.composedPath != "" {
			s.deps.CleanupTempFile(st.composedPath)
		}
		s.challenges.Delete(k)
		return true
	})
}

// ExpireStaleChallenges drops in-memory challenge state for sessions whose
// nonce has passed expires_at. Called from the bot's existing cleanup
// ticker so we don't leak temp files for abandoned sessions.
func (s *Server) ExpireStaleChallenges() {
	now := time.Now()
	s.challenges.Range(func(k, v interface{}) bool {
		st := v.(*challengeState)
		if now.After(st.expiresAt) {
			s.dropChallenge(k.(string))
		}
		return true
	})
}

// serveStatic resolves a path inside the embedded static/ tree and writes
// it as the response body with the given Content-Type.
func (s *Server) serveStatic(w http.ResponseWriter, _ *http.Request, name, contentType string) {
	if strings.Contains(name, "..") {
		http.NotFound(w, nil)
		return
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "static fs error", http.StatusInternalServerError)
		return
	}
	b, err := fs.ReadFile(sub, name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func guessContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	}
	return "application/octet-stream"
}

// Internal sentinel errors translated to opaque HTTP responses so an
// attacker probing the API can't distinguish "bad HMAC" from "expired
// nonce" from "wrong user."
var (
	errUnknownNonce  = errors.New("unknown nonce")
	errSessionClosed = errors.New("session closed")
	errUserMismatch  = errors.New("user mismatch")
)

func writeAuthError(w http.ResponseWriter, _ error) {
	// We deliberately collapse all auth-style errors to a single response.
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[ERROR] miniapp write json: %v", err)
	}
}

// GenerateNonce returns a high-entropy URL-safe nonce suitable for the
// MiniApp sessions table. 32 bytes → 43-char base64url. Exported so the
// chat_join_request handler can mint nonces without importing crypto.
func GenerateNonce() (string, error) {
	return generateNonceBytes(32)
}

// _ here keeps the package buildable while we wire generateNonceBytes from
// the dedicated nonce file (so swapping the source — e.g. for tests — is a
// one-line change).
var _ = fmt.Sprintf
