package proto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client calls a pocketd service. Its zero value is not usable; use
// NewClient.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient returns a client of the service at baseURL ("http://host:port"),
// authenticated with a device token. A nil httpClient uses a client without a
// timeout, which the event stream needs; bound each call with its context.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("service URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("service URL %q: the scheme must be http or https", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{base: base, token: token, http: httpClient}, nil
}

// NewRequestID returns a random request id for a request that changes state.
// Retrying the same request with the same id does nothing twice.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// Path returns pattern with each {id} replaced by id.
func Path(pattern string, id any) string {
	return strings.Replace(pattern, "{id}", url.PathEscape(fmt.Sprint(id)), 1)
}

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var out Hello
	return out, c.do(ctx, http.MethodGet, PathHello, nil, &out)
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out []Model
	return out, c.do(ctx, http.MethodGet, PathModels, nil, &out)
}

func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	var out []Profile
	return out, c.do(ctx, http.MethodGet, PathProfiles, nil, &out)
}

func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var out []Session
	return out, c.do(ctx, http.MethodGet, PathSessions, nil, &out)
}

func (c *Client) CreateSession(ctx context.Context, request CreateSession) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodPost, PathSessions, request, &out)
}

func (c *Client) Session(ctx context.Context, id int64) (SessionDetail, error) {
	var out SessionDetail
	return out, c.do(ctx, http.MethodGet, Path(PathSession, id), nil, &out)
}

func (c *Client) Send(ctx context.Context, id int64, request SendMessage) (SentMessage, error) {
	var out SentMessage
	return out, c.do(ctx, http.MethodPost, Path(PathMessages, id), request, &out)
}

func (c *Client) Abort(ctx context.Context, id int64, request Abort) error {
	return c.do(ctx, http.MethodPost, Path(PathAbort, id), request, nil)
}

// ChangeModel changes a session's model or thinking level (rule 7e).
func (c *Client) ChangeModel(ctx context.Context, id int64, request ChangeModel) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodPost, Path(PathModel, id), request, &out)
}

// StartOver starts a standing session's context afresh (rule 7d).
func (c *Client) StartOver(ctx context.Context, id int64, request StartOver) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodPost, Path(PathStartOver, id), request, &out)
}

func (c *Client) Approvals(ctx context.Context) ([]Approval, error) {
	var out []Approval
	return out, c.do(ctx, http.MethodGet, PathApprovals, nil, &out)
}

func (c *Client) Decide(ctx context.Context, id string, request Decide) (Approval, error) {
	var out Approval
	return out, c.do(ctx, http.MethodPost, Path(PathApproval, id), request, &out)
}

func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	var out []Thread
	return out, c.do(ctx, http.MethodGet, PathThreads, nil, &out)
}

func (c *Client) CloseThread(ctx context.Context, id int64, request CloseThread) (Thread, error) {
	var out Thread
	return out, c.do(ctx, http.MethodPost, Path(PathCloseThread, id), request, &out)
}

// StopShift stops thread id's shift for the owner (ADR-0024).
func (c *Client) StopShift(ctx context.Context, id int64, request StopShift) (Thread, error) {
	var out Thread
	return out, c.do(ctx, http.MethodPost, Path(PathStopShift, id), request, &out)
}

// ResumeShift goes on with thread id's shift for the owner (ADR-0024).
func (c *Client) ResumeShift(ctx context.Context, id int64, request ResumeShift) (Thread, error) {
	var out Thread
	return out, c.do(ctx, http.MethodPost, Path(PathResumeShift, id), request, &out)
}

func (c *Client) Asks(ctx context.Context) ([]Ask, error) {
	var out []Ask
	return out, c.do(ctx, http.MethodGet, PathAsks, nil, &out)
}

func (c *Client) AnswerAsk(ctx context.Context, id int64, request AnswerAsk) (Ask, error) {
	var out Ask
	return out, c.do(ctx, http.MethodPost, Path(PathAsk, id), request, &out)
}

// Lessons lists every agent's lessons that are not gone: in force, or
// waiting for the owner.
func (c *Client) Lessons(ctx context.Context) ([]Lesson, error) {
	var out []Lesson
	return out, c.do(ctx, http.MethodGet, PathLessons, nil, &out)
}

// Pulls lists the pull requests of the threads that are not closed, and
// every open one.
func (c *Client) Pulls(ctx context.Context) ([]PullRequest, error) {
	var out []PullRequest
	return out, c.do(ctx, http.MethodGet, PathPulls, nil, &out)
}

// Pull has pocketd read linked pull request id on GitHub now, with its
// merge base, and answers it with its verdict (rule 45): what backs a
// merge at the owner's click, not at pocketd's last round.
func (c *Client) Pull(ctx context.Context, id int64) (PullRequest, error) {
	var out PullRequest
	return out, c.do(ctx, http.MethodGet, Path(PathPull, id), nil, &out)
}

// Plans lists every plan, the most recently changed first.
func (c *Client) Plans(ctx context.Context) ([]Plan, error) {
	var out []Plan
	return out, c.do(ctx, http.MethodGet, PathPlans, nil, &out)
}

// ApprovePlan approves plan id at the revision and graph version the owner
// saw.
func (c *Client) ApprovePlan(ctx context.Context, id int64, request ApprovePlan) (Plan, error) {
	var out Plan
	return out, c.do(ctx, http.MethodPost, Path(PathApprovePlan, id), request, &out)
}

// DecideLesson approves, discards or forgets lesson id.
func (c *Client) DecideLesson(ctx context.Context, id int64, request DecideLesson) (Lesson, error) {
	var out Lesson
	return out, c.do(ctx, http.MethodPost, Path(PathLesson, id), request, &out)
}

// Terminals lists the owner's shells, oldest first (ADR-0025).
func (c *Client) Terminals(ctx context.Context) ([]Terminal, error) {
	var out []Terminal
	return out, c.do(ctx, http.MethodGet, PathTerminals, nil, &out)
}

// StartTerminal starts a shell and answers its id.
func (c *Client) StartTerminal(ctx context.Context, request StartTerminal) (TerminalStarted, error) {
	var out TerminalStarted
	return out, c.do(ctx, http.MethodPost, PathTerminals, request, &out)
}

// EndTerminal ends shell id.
func (c *Client) EndTerminal(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, Path(PathTerminal, id), nil, nil)
}

// TerminalOutput waits for shell id's output past byte from, at most 25
// seconds, and answers what came, or none; without wait it answers at
// once with what pocketd keeps, the shell's history. ack is how much of it
// the client has drawn, at most from; tab names the client's connection,
// "" for none.
func (c *Client) TerminalOutput(ctx context.Context, id string, from, ack int64, tab string, wait bool) (TerminalOutput, error) {
	q := url.Values{}
	q.Set("from", strconv.FormatInt(from, 10))
	q.Set("ack", strconv.FormatInt(ack, 10))
	if tab != "" {
		q.Set("tab", tab)
	}
	if !wait {
		q.Set("wait", "0")
	}
	var out TerminalOutput
	return out, c.do(ctx, http.MethodGet, Path(PathTerminalOutput, id)+"?"+q.Encode(), nil, &out)
}

// TerminalInput sends keys to shell id.
func (c *Client) TerminalInput(ctx context.Context, id string, request TerminalInput) error {
	return c.do(ctx, http.MethodPost, Path(PathTerminalInput, id), request, nil)
}

// ResizeTerminal tells shell id its size.
func (c *Client) ResizeTerminal(ctx context.Context, id string, request ResizeTerminal) error {
	return c.do(ctx, http.MethodPost, Path(PathTerminalResize, id), request, nil)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set(HeaderProtocol, strconv.Itoa(Version))
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	request, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return decodeError(response)
	}
	if out == nil {
		_, err := io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func decodeError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	e := &Error{Status: response.StatusCode}
	if err := json.Unmarshal(data, e); err != nil || e.Code == "" {
		return &Error{Code: ErrorCodeInternal, Message: strings.TrimSpace(string(data)), Status: response.StatusCode}
	}
	return e
}

// Events reads GET /v1/events until ctx ends, the stream ends or handle
// returns an error, and returns why. The first event is EventHello, at the
// Seq the stream starts from. lastEventID 0 starts at the newest event; a
// later call passes the Seq of the last event it handled, the hello's
// included, and the service resends what came after or sends EventResync.
// Events never retries: the caller reconnects.
func (c *Client) Events(ctx context.Context, lastEventID int64, handle func(Event) error) error {
	request, err := c.newRequest(ctx, http.MethodGet, PathEvents, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return decodeError(response)
	}
	return ReadEvents(response.Body, handle)
}

// ErrStreamEnded is returned by ReadEvents when the stream ends cleanly.
var ErrStreamEnded = errors.New("event stream ended")

// ReadEvents parses a Server-Sent Events stream of Event data and calls
// handle for each event, in order.
func ReadEvents(r io.Reader, handle func(Event) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() == 0 {
				continue
			}
			var event Event
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
				return fmt.Errorf("event data: %w", err)
			}
			data.Reset()
			if err := handle(event); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// "id:", "event:" and ":" comments: the data carries Seq and Type.
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ErrStreamEnded
}

// WriteEvent writes event as one Server-Sent Event.
func WriteEvent(w io.Writer, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, data)
	return err
}
