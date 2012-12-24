package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type TestServer struct {
	cmd      *exec.Cmd
	port     int
	identity string
	stateDir string
}

func NewTestServer(t *testing.T) (*TestServer, error) {
	var err error
	server := new(TestServer)
	if server.stateDir, err = ioutil.TempDir("", "pond-client-test"); err != nil {
		return nil, err
	}
	server.cmd = exec.Command("../server/server",
		"--init",
		"--base-directory", server.stateDir,
		"--port", "0",
	)
	rawStderr, err := server.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := server.cmd.Start(); err != nil {
		return nil, err
	}

	stderr := bufio.NewReader(rawStderr)

	for {
		line, isPrefix, err := stderr.ReadLine()
		if err != nil {
			return nil, errors.New("error while reading status line: " + err.Error())
		}
		if isPrefix {
			continue
		}
		if i := bytes.Index(line, []byte("Started. Listening on port ")); i != -1 {
			line = line[i:]
			words := strings.Split(string(line), " ")
			if len(words) < 8 {
				return nil, errors.New("status line from server has unexpected form: " + string(line))
			}
			portStr := words[4]
			server.identity = words[7]

			if server.port, err = strconv.Atoi(portStr); err != nil {
				return nil, errors.New("failed to parse port number: " + err.Error())
			}
			break
		}
	}

	go func() {
		for {
			line, _, err := stderr.ReadLine()
			if err != nil {
				return
			}
			t.Logf("%s\n", string(line))
		}
	}()

	return server, nil
}

func (server *TestServer) URL() string {
	return fmt.Sprintf("pondserver://%s@127.0.0.1:%d", server.identity, server.port)
}

func (server *TestServer) Close() {
	server.cmd.Process.Kill()
	server.cmd.Wait()
	os.RemoveAll(server.stateDir)
}

type TestUI struct {
	actions        chan interface{}
	events         chan interface{}
	signal         chan chan bool
	currentStateID int
	t              *testing.T
	text           map[string]string
	fileOpen       FileOpen
	haveFileOpen   bool
	panicOnSignal  bool
}

func NewTestUI(t *testing.T) *TestUI {
	return &TestUI{
		actions:        make(chan interface{}, 16),
		events:         make(chan interface{}, 16),
		signal:         make(chan chan bool),
		currentStateID: uiStateInvalid,
		t:              t,
		text:           make(map[string]string),
	}
}

func (ui *TestUI) Actions() chan<- interface{} {
	return ui.actions
}

func (ui *TestUI) Events() <-chan interface{} {
	return ui.events
}

func (ui *TestUI) Signal() {
	c := make(chan bool)
	ui.signal <- c
	<-c
}

func (ui *TestUI) Run() {
	panic("should never be called")
}

func (ui *TestUI) processWidget(widget interface{}) {
	switch v := widget.(type) {
	case VBox:
		for _, child := range v.children {
			ui.processWidget(child)
		}
	case HBox:
		for _, child := range v.children {
			ui.processWidget(child)
		}
	case EventBox:
		ui.processWidget(v.child)
	case Scrolled:
		ui.processWidget(v.child)
	case Frame:
		ui.processWidget(v.child)
	case TextView:
		ui.text[v.name] = v.text
	case Label:
		ui.text[v.name] = v.text
	}
}

func (ui *TestUI) WaitForSignal() error {
	var uierr error
	ack, ok := <-ui.signal
	if !ok {
		panic("signal channel closed")
	}
	ui.t.Logf("Signal")

ReadActions:
	for {
		select {
		case action := <-ui.actions:
			ui.t.Logf("%#v", action)
			switch action := action.(type) {
			case UIState:
				ui.currentStateID = action.stateID
			case UIError:
				uierr = action.err
			case SetText:
				ui.text[action.name] = action.text
			case SetTextView:
				ui.text[action.name] = action.text
			case SetChild:
				ui.processWidget(action.child)
			case Append:
				for _, child := range action.children {
					ui.processWidget(child)
				}
			case FileOpen:
				ui.fileOpen = action
				ui.haveFileOpen = true
			}
		default:
			break ReadActions
		}
	}
	ack <- true

	return uierr
}

func (ui *TestUI) WaitForFileOpen() FileOpen {
	ui.haveFileOpen = false
	for !ui.haveFileOpen {
		if err := ui.WaitForSignal(); err != nil {
			ui.t.Fatal(err)
		}
	}
	return ui.fileOpen
}

type TestClient struct {
	*client
	stateDir string
	ui       *TestUI
}

func NewTestClient(t *testing.T) (*TestClient, error) {
	tc := &TestClient{
		ui: NewTestUI(t),
	}
	var err error
	if tc.stateDir, err = ioutil.TempDir("", "pond-client-test"); err != nil {
		return nil, err
	}
	tc.client = NewClient(filepath.Join(tc.stateDir, "state"), tc.ui, rand.Reader, true, false)
	return tc, nil
}

func (tc *TestClient) Shutdown() {
	tc.ui.t.Log("Shutting down client")
	close(tc.ui.events)

WaitForClient:
	for {
		select {
		case _, ok := <-tc.ui.actions:
			if !ok {
				break WaitForClient
			}
		case ack := <-tc.ui.signal:
			ack <- true
		}
	}
}

func (tc *TestClient) Close() {
	tc.Shutdown()
	os.RemoveAll(tc.stateDir)
}

func (tc *TestClient) AdvanceTo(state int) {
	tc.ui.currentStateID = uiStateInvalid
	for tc.ui.currentStateID != state {
		if err := tc.ui.WaitForSignal(); err != nil {
			tc.ui.t.Fatal(err)
		}
	}
}

func (tc *TestClient) Reload() {
	tc.Shutdown()
	tc.ui = NewTestUI(tc.ui.t)
	tc.client = NewClient(filepath.Join(tc.stateDir, "state"), tc.ui, rand.Reader, true, false)
}

func TestAccountCreation(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	client.ui.WaitForSignal()
	if id := client.ui.currentStateID; id != uiStateLoading {
		t.Fatalf("client in UI state %d when it was expected to be loading", id)
	}

	client.ui.WaitForSignal()
	if id := client.ui.currentStateID; id != uiStateCreatePassphrase {
		t.Fatalf("client in UI state %d when it was expected to be creating a passphrase", id)
	}
	client.ui.events <- Click{
		name:    "next",
		entries: map[string]string{"pw": ""},
	}

	client.ui.WaitForSignal()
	if id := client.ui.currentStateID; id != uiStateCreateAccount {
		t.Fatalf("client in UI state %d when it was expected to be creating an account", id)
	}

	client.ui.events <- Click{
		name:    "create",
		entries: map[string]string{"server": "asldfjksadfkl"},
	}
	t.Log("Waiting for error from garbage URL")
	for {
		if err := client.ui.WaitForSignal(); err != nil {
			break
		}
	}

	url := server.URL()
	client.ui.events <- Click{
		name:    "create",
		entries: map[string]string{"server": url[:len(url)-1]},
	}

	t.Log("Waiting for error from invalid port")
	for {
		if err := client.ui.WaitForSignal(); err != nil {
			break
		}
	}

	t.Log("Waiting for success")
	client.ui.events <- Click{
		name:    "create",
		entries: map[string]string{"server": url},
	}
	client.AdvanceTo(uiStateMain)
}

func proceedToMainUI(t *testing.T, client *TestClient, server *TestServer) {
	client.AdvanceTo(uiStateCreatePassphrase)
	client.ui.events <- Click{
		name:    "next",
		entries: map[string]string{"pw": ""},
	}
	client.AdvanceTo(uiStateCreateAccount)
	url := server.URL()
	client.ui.events <- Click{
		name:    "create",
		entries: map[string]string{"server": url},
	}
	client.AdvanceTo(uiStateMain)
}

func proceedToKeyExchange(t *testing.T, client *TestClient, server *TestServer, otherName string) {
	proceedToMainUI(t, client, server)

	client.ui.events <- Click{name: "newcontact"}
	client.AdvanceTo(uiStateNewContact)

	client.ui.events <- Click{
		name:    "create",
		entries: map[string]string{"name": otherName},
	}
	client.AdvanceTo(uiStateNewContact2)
}

func proceedToPaired(t *testing.T, client1, client2 *TestClient, server *TestServer) {
	proceedToKeyExchange(t, client1, server, "client2")
	proceedToKeyExchange(t, client2, server, "client1")

	client1.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client2.ui.text["kxout"]},
	}
	client1.AdvanceTo(uiStateShowContact)

	client2.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client1.ui.text["kxout"]},
	}
	client2.AdvanceTo(uiStateShowContact)
}

func TestKeyExchange(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client1, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()

	client2, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()

	proceedToKeyExchange(t, client1, server, "client2")
	proceedToKeyExchange(t, client2, server, "client1")

	client1.Reload()
	client1.AdvanceTo(uiStateMain)
	client1.ui.events <- Click{
		name: client1.contactsUI.entries[0].boxName,
	}
	client1.AdvanceTo(uiStateNewContact)
	client2.Reload()
	client2.AdvanceTo(uiStateMain)
	client2.ui.events <- Click{
		name: client2.contactsUI.entries[0].boxName,
	}
	client2.AdvanceTo(uiStateNewContact)

	client1.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": "rubbish"},
	}
	t.Log("Waiting for error from garbage key exchange")
	for {
		if err := client1.ui.WaitForSignal(); err != nil {
			break
		}
	}

	kxBytes := []byte(client2.ui.text["kxout"])
	kxBytes[55] ^= 1
	client1.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": string(kxBytes)},
	}
	t.Log("Waiting for error from corrupt key exchange")
	for {
		if err := client1.ui.WaitForSignal(); err != nil {
			break
		}
	}
	client1.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client2.ui.text["kxout"]},
	}
	client1.AdvanceTo(uiStateShowContact)

	client2.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client1.ui.text["kxout"]},
	}
	client2.AdvanceTo(uiStateShowContact)
}

func contactByName(client *TestClient, name string) (id uint64, contact *Contact) {
	for id, contact = range client.contacts {
		if contact.name == name {
			return
		}
	}
	panic("contact not found: " + name)
}

func selectContact(t *testing.T, client *TestClient, name string) {
	id, _ := contactByName(client, name)
	var boxName string
	for _, item := range client.contactsUI.entries {
		if item.id == id {
			boxName = item.boxName
		}
	}
	if len(boxName) == 0 {
		panic("couldn't find box for given name")
	}
	client.ui.events <- Click{name: boxName}
	client.AdvanceTo(uiStateShowContact)
}

func sendMessage(client *TestClient, to string, message string) {
	client.ui.events <- Click{name: "compose"}
	client.AdvanceTo(uiStateCompose)

	client.ui.events <- Click{
		name:      "send",
		combos:    map[string]string{"to": to},
		textViews: map[string]string{"body": message},
	}

	client.AdvanceTo(uiStateOutbox)
	ackChan := make(chan bool)
	client.fetchNowChan <- ackChan
	<-ackChan
}

func fetchMessage(client *TestClient) (from string, msg *InboxMessage) {
	ackChan := make(chan bool)
	client.fetchNowChan <- ackChan

WaitForAck:
	for {
		select {
		case ack := <-client.ui.signal:
			ack <- true
		case <-ackChan:
			break WaitForAck
		}
	}

	if len(client.inbox) == 0 {
		panic("no messages")
	}
	msg = client.inbox[len(client.inbox)-1]
	from = client.contacts[msg.from].name
	return
}

func TestMessageExchange(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client1, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()

	client2, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()

	proceedToPaired(t, client1, client2, server)

	var initialCurrentDH [32]byte
	for _, contact := range client1.contacts {
		if contact.name == "client2" {
			copy(initialCurrentDH[:], contact.currentDHPrivate[:])
		}
	}

	for i := 0; i < 3; i++ {
		testMsg := fmt.Sprintf("test message %d", i)
		sendMessage(client1, "client2", testMsg)
		from, msg := fetchMessage(client2)
		if from != "client1" {
			t.Fatalf("message from %s, expected client1", from)
		}
		if string(msg.message.Body) != testMsg {
			t.Fatalf("Incorrect message contents: %s", msg)
		}

		sendMessage(client2, "client1", testMsg)
		from, msg = fetchMessage(client1)
		if from != "client2" {
			t.Fatalf("message from %s, expected client2", from)
		}
		if string(msg.message.Body) != testMsg {
			t.Fatalf("Incorrect message contents: %s", msg)
		}
	}

	// Ensure that the DH secrets are advancing.
	for _, contact := range client1.contacts {
		if contact.name == "client2" {
			if bytes.Equal(initialCurrentDH[:], contact.currentDHPrivate[:]) {
				t.Fatalf("DH secrets aren't advancing!")
			}
		}
	}
}

func TestACKs(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client1, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()

	client2, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()

	proceedToPaired(t, client1, client2, server)

	const testMsg = "test message"
	sendMessage(client1, "client2", testMsg)
	from, msg := fetchMessage(client2)
	if from != "client1" {
		t.Fatalf("message from %s, expected client1", from)
	}
	if string(msg.message.Body) != testMsg {
		t.Fatalf("Incorrect message contents: %s", msg)
	}
	if !client1.outbox[0].acked.IsZero() {
		t.Fatalf("client1 incorrectly believes that its message has been acked")
	}
	client2.ui.events <- Click{
		name: client2.inboxUI.entries[0].boxName,
	}
	client2.AdvanceTo(uiStateInbox)
	client2.ui.events <- Click{
		name: "ack",
	}
	client2.AdvanceTo(uiStateInbox)

	ackChan := make(chan bool)
	client2.fetchNowChan <- ackChan
	<-ackChan

	from, _ = fetchMessage(client1)
	if from != "client2" {
		t.Fatalf("ack received from wrong contact: %s", from)
	}

	if client1.outbox[0].acked.IsZero() {
		t.Fatalf("client1 doesn't believe that its message has been acked")
	}
}

func TestHalfPairedMessageExchange(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client1, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()

	client2, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()

	proceedToKeyExchange(t, client1, server, "client2")
	proceedToKeyExchange(t, client2, server, "client1")

	client1KX := client1.ui.text["kxout"]
	client1.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client2.ui.text["kxout"]},
	}
	client1.AdvanceTo(uiStateShowContact)

	// Now client1 is paired with client2, but client2 is still pending on
	// client1.

	// Send a message from client1 to client2.
	const testMsg = "test message"
	sendMessage(client1, "client2", testMsg)
	from, msg := fetchMessage(client2)
	if from != "client1" {
		t.Fatalf("message from %s, expected client1", from)
	}
	if len(msg.sealed) == 0 {
		t.Fatalf("no sealed message from client2")
	}
	if len(client2.inboxUI.entries) == 0 {
		t.Fatalf("no pending UI entry in client2")
	}

	// Check that viewing the message in client2 doesn't crash anything.
	client2.ui.events <- Click{
		name: client2.inboxUI.entries[0].boxName,
	}
	client2.AdvanceTo(uiStateInbox)

	client2.Reload()
	client2.AdvanceTo(uiStateMain)

	// Select the pending contact in client2 to complete the key exchange.
	client2.ui.events <- Click{
		name: client2.contactsUI.entries[0].boxName,
	}
	client2.AdvanceTo(uiStateNewContact)
	client2.ui.events <- Click{
		name:      "process",
		textViews: map[string]string{"kxin": client1KX},
	}
	client2.AdvanceTo(uiStateShowContact)
	client2.ui.events <- Click{
		name: client2.inboxUI.entries[0].boxName,
	}
	client2.AdvanceTo(uiStateInbox)

	if s := client2.ui.text["body"]; s != testMsg {
		t.Fatalf("resolved message is incorrect: %s", s)
	}
}

func TestDraft(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	proceedToMainUI(t, client, server)
	client.ui.events <- Click{name: "compose"}
	client.AdvanceTo(uiStateCompose)

	if l := len(client.drafts); l != 1 {
		t.Fatalf("Bad number of drafts: %d", l)
	}
	var draftID uint64
	for id := range client.drafts {
		draftID = id
		break
	}

	const initialText = "wibble wobble"

	client.ui.events <- Update{
		name: "body",
		text: initialText,
	}
	client.Reload()
	client.AdvanceTo(uiStateMain)

	if l := len(client.drafts); l != 1 {
		t.Fatalf("Bad number of drafts after reload: %d", l)
	}

	if l := len(client.draftsUI.entries); l != 1 {
		t.Fatalf("Bad number of draft UI entries after reload: %d", l)
	}

	if id := client.draftsUI.entries[0].id; id != draftID {
		t.Fatalf("Incorrect draft ID after reload: %d vs %d", id, draftID)
	}

	client.ui.events <- Click{name: client.draftsUI.entries[0].boxName}
	client.AdvanceTo(uiStateCompose)
	if text := client.ui.text["body"]; text != initialText {
		t.Fatalf("Wrong message text after reload: '%s' vs '%s'", text, initialText)
	}

	attachmentFile := filepath.Join(client.stateDir, "attachment")
	if err := ioutil.WriteFile(attachmentFile, []byte(initialText), 0644); err != nil {
		t.Fatalf("Failed to write attachment file: %s", err)
	}

	client.ui.events <- Click{name: "attach"}
	client.ui.events <- OpenResult{path: attachmentFile, ok: true}

	client.Reload()
	client.AdvanceTo(uiStateMain)
	client.ui.events <- Click{name: client.draftsUI.entries[0].boxName}
	client.AdvanceTo(uiStateCompose)

	const labelPrefix = "attachment-label-"
	var attachmentID uint64
	for name := range client.ui.text {
		if strings.HasPrefix(name, labelPrefix) {
			attachmentID, err = strconv.ParseUint(name[len(labelPrefix):], 16, 64)
			if err != nil {
				t.Fatalf("Failed to parse attachment label: %s", name)
			}
			break
		}
	}

	if attachmentID == 0 {
		t.Fatalf("failed to find attachment after reload")
	}

	client.ui.events <- Click{name: fmt.Sprintf("remove-%x", attachmentID)}
	client.Reload()
	client.AdvanceTo(uiStateMain)
	client.ui.events <- Click{name: client.draftsUI.entries[0].boxName}
	client.AdvanceTo(uiStateCompose)

	for name := range client.ui.text {
		if strings.HasPrefix(name, labelPrefix) {
			t.Fatalf("Found attachment after removing")
		}
	}

	errorFile := filepath.Join(client.stateDir, "error")
	if err := ioutil.WriteFile(errorFile, nil, 0); err != nil {
		t.Fatalf("Failed to write error file: %s", err)
	}

	client.ui.events <- Click{name: "attach"}
	client.ui.WaitForFileOpen()
	client.ui.events <- OpenResult{path: attachmentFile, ok: true}
	client.ui.events <- Click{name: "attach"}
	client.ui.WaitForFileOpen()
	client.ui.events <- OpenResult{path: errorFile, ok: true}
	client.ui.WaitForSignal()

	attachmentID = 0
	const errorPrefix = "attachment-error-"
	for name := range client.ui.text {
		if strings.HasPrefix(name, errorPrefix) {
			attachmentID, err = strconv.ParseUint(name[len(errorPrefix):], 16, 64)
			if err != nil {
				t.Fatalf("Failed to parse attachment label: %s", name)
			}
			break
		}
	}

	if attachmentID == 0 {
		t.Fatalf("failed to find error attachment")
	}

	client.ui.events <- Click{name: fmt.Sprintf("remove-%x", attachmentID)}
	client.ui.WaitForSignal()
}

func TestDetachedFiles(t *testing.T) {
	t.Parallel()

	server, err := NewTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client1, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client1.Close()

	client2, err := NewTestClient(t)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()

	proceedToPaired(t, client1, client2, server)
	client1.ui.events <- Click{name: "compose"}
	client1.AdvanceTo(uiStateCompose)

	plaintextPath := filepath.Join(client1.stateDir, "file")
	ciphertextPath := filepath.Join(client1.stateDir, "encrypted")
	plaintext := make([]byte, 20*1024)
	io.ReadFull(rand.Reader, plaintext)
	if err := ioutil.WriteFile(plaintextPath, plaintext, 0644); err != nil {
		t.Fatal(err)
	}

	client1.ui.events <- Click{name: "attach"}
	client1.ui.WaitForFileOpen()
	client1.ui.events <- OpenResult{path: plaintextPath, ok: true}
	client1.ui.WaitForSignal()
	for name := range client1.ui.text {
		const labelPrefix = "attachment-label-"
		if strings.HasPrefix(name, labelPrefix) {
			attachmentID, err := strconv.ParseUint(name[len(labelPrefix):], 16, 64)
			if err != nil {
				t.Fatalf("Failed to parse attachment label: %s", name)
			}
			client1.ui.events <- Click{name: fmt.Sprintf("attachment-convert-%x", attachmentID)}
			break
		}
	}
	fo := client1.ui.WaitForFileOpen()
	client1.ui.events <- OpenResult{path: ciphertextPath, ok: true, arg: fo.arg}
	client1.ui.WaitForSignal()

	var draft *Draft
	for _, d := range client1.drafts {
		draft = d
		break
	}

	for len(draft.detachments) == 0 {
		client1.ui.WaitForSignal()
	}

	client1.ui.events <- Click{
		name:      "send",
		combos:    map[string]string{"to": "client2"},
		textViews: map[string]string{"body": "foo"},
	}

	client1.AdvanceTo(uiStateOutbox)
	ackChan := make(chan bool)
	client1.fetchNowChan <- ackChan
	<-ackChan

	_, msg := fetchMessage(client2)
	if len(msg.message.DetachedFiles) != 1 {
		t.Fatalf("message received with no detachments")
	}

	for _, e := range client2.inboxUI.entries {
		if e.id == msg.id {
			client2.ui.events <- Click{name: e.boxName}
			break
		}
	}

	client2.AdvanceTo(uiStateInbox)
	client2.ui.events <- Click{name: "detachment-decrypt-0"}
	fo = client2.ui.WaitForFileOpen()
	client2.ui.events <- OpenResult{ok: true, path: ciphertextPath, arg: fo.arg}
	fo = client2.ui.WaitForFileOpen()
	outputPath := filepath.Join(client1.stateDir, "output")
	client2.ui.events <- OpenResult{ok: true, path: outputPath, arg: fo.arg}
	client2.ui.WaitForSignal()

	var id uint64
	for dID := range msg.decryptions {
		id = dID
		break
	}

	if id == 0 {
		t.Fatalf("Failed to get id of decryption")
	}

	for len(msg.decryptions) > 0 {
		client2.ui.WaitForSignal()
	}

	result, err := ioutil.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(result, plaintext) {
		t.Fatalf("bad decryption")
	}
}
