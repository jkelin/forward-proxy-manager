package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/sync/semaphore"
)

// cfMitigatedHeader is Cloudflare's marker on its challenge pages ("challenge").
const cfMitigatedHeader = "Cf-Mitigated"

// challengeStatusHeader tells the client how a Cloudflare challenge on this request was handled.
const challengeStatusHeader = "X-Fpm-Challenge"

const (
	// challengeSolved: the solver cleared a challenge for this request; the body is the protected page.
	challengeSolved = "solved"
	// challengeReused: the solver identity answered with an earlier clearance on this exit.
	challengeReused = "reused"
	// challengeUnchallenged: the solver identity passed without a clearance.
	challengeUnchallenged = "unchallenged"
	// challengeFailed: nothing passed; the response is the last challenge or block.
	challengeFailed = "failed"
)

// isChallenge reports whether response is a Cloudflare challenge page (403/503 with "cf-mitigated: challenge").
func isChallenge(response *Response) bool {
	isChallengeStatus := response.Code == http.StatusForbidden || response.Code == http.StatusServiceUnavailable

	return response.Status == ResponseStatusOk && isChallengeStatus && response.Headers.Get(cfMitigatedHeader) == "challenge"
}

// solverIdentity is the browser the solver impersonates. A clearance only works for requests with the same IP,
// user agent and TLS fingerprint, so hosts the solver passed are fetched with this identity afterwards.
type solverIdentity struct {
	profilePath string
	tlsProfile  profiles.ClientProfile
	// headers are the typed-URL navigation headers, in the browser's order.
	headers fhttp.Header
}

// solverTlsProfiles maps cfsolve transport profiles (minor version dropped) to matching tls-client profiles.
// Measured: a cf_clearance from cfsolve's safari_ios_26.2 passes with tls-client's Safari_IOS_26_0.
var solverTlsProfiles = map[string]profiles.ClientProfile{
	"safari_ios_26": profiles.Safari_IOS_26_0,
}

var minorVersionSuffix = regexp.MustCompile(`\.\d+$`)

// solverProfileFile is the part of a cfsolve profile file the proxy needs.
type solverProfileFile struct {
	Transport struct {
		Profile  string            `json:"profile"`
		Headers  map[string]string `json:"headers"`
		Requests struct {
			Headers struct {
				Navigation []string `json:"navigation"`
			} `json:"headers"`
		} `json:"requests"`
	} `json:"transport"`
}

// loadSolverIdentity reads the cfsolve profile file at profilePath.
func loadSolverIdentity(profilePath string) (*solverIdentity, error) {
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, err
	}

	return parseSolverIdentity(profilePath, raw)
}

// parseSolverIdentity builds the identity from a cfsolve profile file's contents.
// Navigation entries are "name: value" or a bare name filled from transport.headers (only user-agent is filled).
// Edge cases: unknown transport profiles fail, since their clearances would not work; a missing user agent or
// navigation list fails; other bare names (e.g. referer) have no value on a typed-URL navigation and are skipped.
func parseSolverIdentity(profilePath string, raw []byte) (*solverIdentity, error) {
	var file solverProfileFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("cfsolve profile %s: %w", profilePath, err)
	}

	transport := file.Transport
	profileKey := minorVersionSuffix.ReplaceAllString(transport.Profile, "")

	tlsProfile, known := solverTlsProfiles[profileKey]
	if !known {
		return nil, fmt.Errorf("cfsolve profile %s: transport profile %q has no matching TLS profile", profilePath, transport.Profile)
	}

	userAgent := ""
	for name, value := range transport.Headers {
		if strings.EqualFold(name, "user-agent") {
			userAgent = value
		}
	}

	if userAgent == "" {
		return nil, fmt.Errorf("cfsolve profile %s: transport.headers has no User-Agent", profilePath)
	}

	if len(transport.Requests.Headers.Navigation) == 0 {
		return nil, fmt.Errorf("cfsolve profile %s: transport.requests.headers.navigation is empty", profilePath)
	}

	headers := fhttp.Header{}
	order := []string{}

	for _, entry := range transport.Requests.Headers.Navigation {
		name, value, hasValue := strings.Cut(entry, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)

		if !hasValue {
			if name != "user-agent" {
				continue
			}

			value = userAgent
		}

		headers[name] = []string{value}
		order = append(order, name)
	}

	headers[fhttp.HeaderOrderKey] = order

	return &solverIdentity{profilePath: profilePath, tlsProfile: tlsProfile, headers: headers}, nil
}

// challengeClearance is a solved challenge: the cookies to keep and the protected page.
type challengeClearance struct {
	cookies []*fhttp.Cookie
	page    *Response
}

// challengeSolver clears the challenge on target, fetching through proxyUrl.
type challengeSolver interface {
	clear(ctx context.Context, target *url.URL, proxyUrl string) (*challengeClearance, error)
}

// challengeConfig is shared by every exit.
type challengeConfig struct {
	solver   challengeSolver
	identity *solverIdentity
}

// exitChallenges is one exit's challenge state: the solver identity's client and the hosts it serves.
type exitChallenges struct {
	config *challengeConfig
	// proxyUrl is the exit's proxy URL with credentials; the solver must use the same IP.
	proxyUrl   string
	httpClient tls_client.HttpClient
	// solveLock makes concurrent challenges on one exit wait for a single solve.
	solveLock sync.Mutex
	hostsLock sync.Mutex
	hosts     map[string]struct{}
}

// enableChallenges adds challenge handling to client; proxyUrl must be the proxy client's own transport.
func (client *ProxyClient) enableChallenges(config *challengeConfig, proxyUrl string) error {
	httpClient, err := newIdentityClient(config.identity.tlsProfile, newSessionJar(), client.transportOptions)
	if err != nil {
		return err
	}

	client.challenges = &exitChallenges{
		config:     config,
		proxyUrl:   proxyUrl,
		httpClient: httpClient,
		hosts:      make(map[string]struct{}),
	}

	return nil
}

// serves reports whether host is fetched with the solver identity on this exit.
func (challenges *exitChallenges) serves(host string) bool {
	challenges.hostsLock.Lock()
	defer challenges.hostsLock.Unlock()

	_, served := challenges.hosts[host]

	return served
}

func (challenges *exitChallenges) setServed(host string, served bool) {
	challenges.hostsLock.Lock()
	defer challenges.hostsLock.Unlock()

	if served {
		challenges.hosts[host] = struct{}{}
	} else {
		delete(challenges.hosts, host)
	}
}

// send fetches target with the solver identity and its cookies.
func (challenges *exitChallenges) send(ctx context.Context, method string, target *url.URL) (*Response, error) {
	return send(ctx, challenges.httpClient, method, target, challenges.config.identity.headers)
}

// pass answers a challenge the request got, returning the response to use and its challengeStatusHeader value.
// A challenge from the browser identity is first retried with the solver identity: some exits challenge one
// fingerprint and not the other, and an earlier solve on this exit may cover the host. Only then the solver runs,
// bounded by callerCtx instead of the attempt timeout, since a solve takes several requests.
// A host is served by the solver identity after any answer below 400.
// Edge cases: a challenge from the solver identity means its clearance expired, so it solves again; a transport
// error on the retry or a failed solve returns challenged unchanged as "failed"; a non-challenge 4xx/5xx on the
// retry is returned as "failed" without serving the host.
func (challenges *exitChallenges) pass(callerCtx context.Context, attemptCtx context.Context, method string, target *url.URL, challenged *Response, fromSolverIdentity bool) (*Response, string) {
	challenges.solveLock.Lock()
	defer challenges.solveLock.Unlock()

	host := target.Hostname()

	if fromSolverIdentity {
		challenges.setServed(host, false)
	} else {
		status := challengeUnchallenged
		if challenges.serves(host) {
			status = challengeReused
		}

		response, err := challenges.send(attemptCtx, method, target)
		if err != nil {
			log.Printf("challenge retry %s: %v", target, err)

			return challenged, challengeFailed
		}

		if !isChallenge(response) {
			if response.Code >= 400 {
				return response, challengeFailed
			}

			challenges.setServed(host, true)

			return response, status
		}

		challenged = response
	}

	clearance, err := challenges.config.solver.clear(callerCtx, target, challenges.proxyUrl)
	if err != nil {
		log.Printf("challenge solve %s: %v", target, err)

		return challenged, challengeFailed
	}

	challenges.httpClient.SetCookies(target, clearance.cookies)

	if clearance.page.Code < 400 {
		challenges.setServed(host, true)
	}

	return clearance.page, challengeSolved
}

// processSolver runs the fpm-cfsolve binary once per challenge (see solver/src/main.rs for the contract).
type processSolver struct {
	binary      string
	profilePath string
	timeout     time.Duration
	// slots bounds concurrent solves; each one deobfuscates and runs the challenge script.
	slots *semaphore.Weighted
}

func newProcessSolver(binary string, profilePath string, timeout time.Duration, concurrency int64) (*processSolver, error) {
	if timeout <= 0 || concurrency <= 0 {
		return nil, fmt.Errorf("invalid solver limits: timeout %s, concurrency %d", timeout, concurrency)
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}

	return &processSolver{binary: path, profilePath: profilePath, timeout: timeout, slots: semaphore.NewWeighted(concurrency)}, nil
}

type solverRequest struct {
	Url     string `json:"url"`
	Proxy   string `json:"proxy"`
	Profile string `json:"profile"`
}

// clear runs the binary; the proxy URL goes over stdin so credentials never show in the process list.
// Edge cases: waiting for a slot and the run both stop when ctx ends; the run also stops after the solver timeout.
func (solver *processSolver) clear(ctx context.Context, target *url.URL, proxyUrl string) (*challengeClearance, error) {
	if err := solver.slots.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer solver.slots.Release(1)

	runCtx, cancel := context.WithTimeout(ctx, solver.timeout)
	defer cancel()

	input, err := json.Marshal(solverRequest{Url: target.String(), Proxy: proxyUrl, Profile: solver.profilePath})
	if err != nil {
		return nil, err
	}

	var stdout bytes.Buffer

	command := exec.CommandContext(runCtx, solver.binary)
	command.Stdin = bytes.NewReader(input)
	command.Stdout = &stdout

	runErr := command.Run()
	if runCtx.Err() != nil {
		return nil, fmt.Errorf("solver stopped: %w", runCtx.Err())
	}

	clearance, err := parseSolverAnswer(stdout.Bytes())
	if err != nil && runErr != nil && !errors.As(err, new(*solverFailure)) {
		return nil, fmt.Errorf("%w (%v)", err, runErr)
	}

	return clearance, err
}

// solverFailure is a solve the binary reported as failed; kind is cfsolve's error class (e.g. "rejected").
type solverFailure struct {
	kind    string
	message string
}

func (failure *solverFailure) Error() string {
	return fmt.Sprintf("cfsolve %s: %s", failure.kind, failure.message)
}

type solverAnswer struct {
	Outcome string         `json:"outcome"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Cookies []solverCookie `json:"cookies"`
	Page    *solverPage    `json:"page"`
}

type solverCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   *string `json:"domain"`
	Path     *string `json:"path"`
	Secure   bool    `json:"secure"`
	HttpOnly bool    `json:"httpOnly"`
	// Expires is Unix seconds; null for session cookies.
	Expires *int64 `json:"expires"`
}

type solverPage struct {
	Url    string `json:"url"`
	Status int    `json:"status"`
	Html   string `json:"html"`
}

// parseSolverAnswer decodes the binary's stdout line.
// Edge cases: a "failed" answer returns *solverFailure; empty output, unknown outcomes, a cleared answer without
// page, an invalid page status or a nameless cookie fail.
func parseSolverAnswer(raw []byte) (*challengeClearance, error) {
	var answer solverAnswer
	if err := json.Unmarshal(bytes.TrimSpace(raw), &answer); err != nil {
		return nil, fmt.Errorf("invalid solver answer: %w", err)
	}

	switch answer.Outcome {
	case "failed":
		return nil, &solverFailure{kind: answer.Kind, message: answer.Message}
	case "cleared":
	default:
		return nil, fmt.Errorf("invalid solver answer: outcome %q", answer.Outcome)
	}

	if answer.Page == nil || answer.Page.Status < 100 || answer.Page.Status > 599 {
		return nil, errors.New("invalid solver answer: cleared without a valid page")
	}

	cookies := make([]*fhttp.Cookie, 0, len(answer.Cookies))

	for _, cookie := range answer.Cookies {
		if cookie.Name == "" {
			return nil, errors.New("invalid solver answer: cookie without name")
		}

		converted := &fhttp.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Secure:   cookie.Secure,
			HttpOnly: cookie.HttpOnly,
		}

		if cookie.Domain != nil {
			converted.Domain = *cookie.Domain
		}

		if cookie.Path != nil {
			converted.Path = *cookie.Path
		}

		if cookie.Expires != nil {
			converted.Expires = time.Unix(*cookie.Expires, 0)
		}

		cookies = append(cookies, converted)
	}

	page := &Response{
		Status:  ResponseStatusOk,
		Code:    answer.Page.Status,
		Body:    []byte(answer.Page.Html),
		Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
	}

	return &challengeClearance{cookies: cookies, page: page}, nil
}
