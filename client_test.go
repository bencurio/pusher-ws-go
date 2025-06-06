package pusher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TODO Implement Client.Error channel in all tests and fail the test if any
// errors occur

func TestUnmarshalDataString(t *testing.T) {
	dest := map[string]interface{}{}
	err := UnmarshalDataString(json.RawMessage(`"{\"foo\":\"A\",\"bar\":1}"`), &dest)
	if err != nil {
		t.Errorf("Expected error to be `nil`, got %v", err)
	}

	wantData := map[string]interface{}{
		"foo": "A",
		"bar": 1.0,
	}
	if !reflect.DeepEqual(dest, wantData) {
		t.Errorf("Expected dest to deep-equal %+v, got %+v", wantData, dest)
	}
}

func TestClientIsConnected(t *testing.T) {
	t.Run("false", func(t *testing.T) {
		client := &Client{connected: false}
		if isConnected := client.isConnected(); isConnected != false {
			t.Errorf("Expected isConnected to return false, got %v", isConnected)
		}
	})
	t.Run("true", func(t *testing.T) {
		client := &Client{connected: true}
		if isConnected := client.isConnected(); isConnected != true {
			t.Errorf("Expected isConnected to return true, got %v", isConnected)
		}
	})
}

func TestClientResetActivityTimer(t *testing.T) {
	client := &Client{
		activityTimerReset: make(chan struct{}, 1),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		<-client.activityTimerReset
		wg.Done()
	}()

	client.resetActivityTimer()

	wg.Wait()
}

func TestClientSendError(t *testing.T) {
	errChan := make(chan error)
	wantErr := errors.New("foo")
	client := &Client{Errors: errChan}

	var gotErr error
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() { gotErr = <-errChan; wg.Done() }()
	runtime.Gosched()
	client.sendError(wantErr)
	wg.Wait()

	if !reflect.DeepEqual(gotErr, wantErr) {
		t.Errorf("Expected to value from error chan to be %+v, got %+v", wantErr, gotErr)
	}
}

func TestClientBind(t *testing.T) {
	wantChan := "foo"
	client := Client{boundEvents: map[string]boundEventChans{}}
	boundChan := client.Bind(wantChan)

	eventBoundChans, ok := client.boundEvents[wantChan]
	if !ok {
		t.Errorf("Expected client bound events to contain %q, got %+v instead", wantChan, client.boundEvents)
	}
	_, ok = eventBoundChans[boundChan]
	if !ok {
		t.Errorf("Expected event bound channels to contain returned channel, got %+v instead", eventBoundChans)
	}
}

func TestClientUnbind(t *testing.T) {
	wantChan := "foo"
	t.Run("eventOnly", func(t *testing.T) {
		client := Client{boundEvents: map[string]boundEventChans{
			wantChan: {make(chan Event): struct{}{}},
		}}
		client.Unbind(wantChan)

		if _, ok := client.boundEvents[wantChan]; ok {
			t.Errorf("Expected client bound events not to contain %q, got %+v instead", wantChan, client.boundEvents)
		}
	})

	t.Run("eventWithChans", func(t *testing.T) {
		wantChan := "foo"
		ch1 := make(chan Event)
		ch2 := make(chan Event)
		ch3 := make(chan Event)
		client := Client{boundEvents: map[string]boundEventChans{
			wantChan: {
				ch1: struct{}{},
				ch2: struct{}{},
				ch3: struct{}{},
			},
		}}
		client.Unbind(wantChan, ch1, ch3)

		eventBoundChans, ok := client.boundEvents[wantChan]
		if !ok {
			t.Errorf("Expected client bound events to contain %q, got %+v instead", wantChan, client.boundEvents)
		}
		_, ok = eventBoundChans[ch1]
		if ok {
			t.Errorf("Expected event bound channels not to contain ch1, got %+v instead", eventBoundChans)
		}
		_, ok = eventBoundChans[ch3]
		if ok {
			t.Errorf("Expected event bound channels not to contain ch3, got %+v instead", eventBoundChans)
		}
		_, ok = eventBoundChans[ch2]
		if !ok {
			t.Errorf("Expected event bound channels to contain ch3, got %+v instead", eventBoundChans)
		}
	})
}

func TestClientSendEvent(t *testing.T) {
	wantEvent := Event{
		Channel: "foo",
		Event:   "bar",
		Data:    json.RawMessage(`{"bar":1,"foo":"A"}`),
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		event := Event{}
		err := websocket.JSON.Receive(ws, &event)
		if err != nil {
			panic(err)
		}
		if !reflect.DeepEqual(event, wantEvent) {
			t.Errorf("Expected received event to deep-equal %+v, got %+v", wantEvent, event)
		}
		wg.Done()
	}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	ws, err := websocket.Dial(wsURL, "ws", localOrigin)
	if err != nil {
		panic(err)
	}

	client := &Client{
		ws:                 ws,
		activityTimerReset: make(chan struct{}, 1),
	}
	defer client.Disconnect()

	err = client.SendEvent(wantEvent.Event, wantEvent.Data, wantEvent.Channel)
	if err != nil {
		panic(err)
	}
	wg.Wait()

	<-client.activityTimerReset
}

func TestClientSubscribe(t *testing.T) {
	t.Run("existingSubscription", func(t *testing.T) {
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		channelName := "foo"
		ch := &channel{name: channelName, subscribed: true}
		client := &Client{
			subscribedChannels: map[string]internalChannel{channelName: ch},
			ws:                 ws,
		}
		defer client.Disconnect()
		ch.client = client
		subCh, err := client.Subscribe(channelName)
		if err != nil {
			panic(err)
		}

		if ch != subCh.(*channel) {
			t.Errorf("Expected subCh to be %+v, got %+v", ch, subCh)
		}
	})

	t.Run("newPublicChannel", func(t *testing.T) {
		channelName := "foo"

		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			var evt Event
			err := websocket.JSON.Receive(ws, &evt)
			if err != nil {
				panic(err)
			}
			if evt.Event != pusherSubscribe {
				t.Errorf("Expected event to be %q, got %q", pusherSubscribe, evt.Event)
			}

			err = websocket.JSON.Send(ws, Event{
				Event:   pusherInternalSubSucceeded,
				Channel: channelName,
			})
			if err != nil {
				panic(err)
			}
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			subscribedChannels: map[string]internalChannel{},
			ws:                 ws,
			connected:          true,
		}
		defer client.Disconnect()

		go client.listen()

		subCh, err := client.Subscribe(channelName)
		if err != nil {
			panic(err)
		}

		baseCh := subCh.(*channel)
		if baseCh.name != channelName {
			t.Errorf("Expected channel name to be %q, got %q", channelName, baseCh.name)
		}
	})

	t.Run("newPrivateChannel", func(t *testing.T) {
		channelName := "private-foo"

		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			var evt Event
			err := websocket.JSON.Receive(ws, &evt)
			if err != nil {
				panic(err)
			}
			if evt.Event != pusherSubscribe {
				t.Errorf("Expected event to be %q, got %q", pusherSubscribe, evt.Event)
			}

			err = websocket.JSON.Send(ws, Event{
				Event:   pusherInternalSubSucceeded,
				Channel: channelName,
			})
			if err != nil {
				panic(err)
			}
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}))

		client := &Client{
			subscribedChannels: map[string]internalChannel{},
			ws:                 ws,
			connected:          true,
			AuthURL:            authSrv.URL,
		}
		defer client.Disconnect()

		go client.listen()

		subCh, err := client.Subscribe(channelName)
		if err != nil {
			panic(err)
		}

		baseCh := subCh.(*privateChannel)
		if baseCh.name != channelName {
			t.Errorf("Expected channel name to be %q, got %q", channelName, baseCh.name)
		}
	})

	t.Run("newPresenceChannel", func(t *testing.T) {
		channelName := "presence-foo"

		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			var evt Event
			err := websocket.JSON.Receive(ws, &evt)
			if err != nil {
				panic(err)
			}
			if evt.Event != pusherSubscribe {
				t.Errorf("Expected event to be %q, got %q", pusherSubscribe, evt.Event)
			}

			data, err := json.Marshal(`
				{
					"presence": {
						"ids": ["1", "2"],
						"hash": {
							"1": { "name": "name-1" },
							"2": { "name": "name-2" }
						},
						"count": 2
					}
				}
			`)
			if err != nil {
				t.Fatal("error marshaling data: ", err)
			}

			err = websocket.JSON.Send(ws, Event{
				Event:   pusherInternalSubSucceeded,
				Channel: channelName,
				Data:    data,
			})
			if err != nil {
				panic(err)
			}
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}))

		client := &Client{
			subscribedChannels: map[string]internalChannel{},
			ws:                 ws,
			connected:          true,
			AuthURL:            authSrv.URL,
		}
		defer client.Disconnect()

		go client.listen()

		subCh, err := client.SubscribePresence(channelName)
		if err != nil {
			panic(err)
		}

		baseCh := subCh.(*presenceChannel)
		if baseCh.name != channelName {
			t.Errorf("Expected channel name to be %q, got %q", channelName, baseCh.name)
		}
	})
}

func TestClientUnsubscribe(t *testing.T) {
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	ws, err := websocket.Dial(wsURL, "ws", localOrigin)
	if err != nil {
		panic(err)
	}

	ch := &channel{name: "foo"}
	client := &Client{
		subscribedChannels: map[string]internalChannel{"foo": ch},
		ws:                 ws,
	}
	defer client.Disconnect()
	ch.client = client
	err = client.Unsubscribe("foo")
	if err != nil {
		panic(err)
	}

	if _, ok := client.subscribedChannels["foo"]; ok {
		t.Errorf("Expected client subscribed channels not to contain 'foo', got %+v", client.subscribedChannels)
	}
}

func TestClientDisconnect(t *testing.T) {
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
	defer srv.Close()
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	ws, err := websocket.Dial(wsURL, "ws", localOrigin)
	if err != nil {
		panic(err)
	}

	client := &Client{
		connected: true,
		ws:        ws,
	}

	err = client.Disconnect()
	if err != nil {
		panic(err)
	}

	if client.connected {
		t.Errorf("Expected client connected to be false, got true")
	}
	if err = ws.Close(); err == nil {
		t.Errorf("Expected websocket connection to have been closed, got no error closing again")
	}
}

func TestClientHeartbeat(t *testing.T) {
	t.Run("notConnected", func(t *testing.T) {
		timeChan := make(chan time.Time)
		client := &Client{
			connected:          false,
			activityTimerReset: make(chan struct{}, 1),
			activityTimer:      &time.Timer{C: timeChan},
		}

		go func() {
			client.activityTimerReset <- struct{}{}
			client.activityTimerReset <- struct{}{}
			t.Errorf("Expected not to block on send to activityTimerReset, but message was received")
		}()
		go func() {
			timeChan <- time.Now()
			t.Errorf("Expected not to block on send to activityTimer chan, but message was received")
		}()

		client.heartbeat()
	})

	t.Run("timerReset", func(t *testing.T) {
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			connected:          true,
			activityTimerReset: make(chan struct{}, 1),
			activityTimer:      time.NewTimer(1 * time.Hour),
			activityTimeout:    0,
			ws:                 ws,
		}

		go client.heartbeat()
		runtime.Gosched()

		client.Disconnect()
		client.activityTimerReset <- struct{}{}

		<-client.activityTimer.C
	})

	t.Run("timerExpire", func(t *testing.T) {
		wg := &sync.WaitGroup{}
		wg.Add(1)
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			var event Event
			err := websocket.JSON.Receive(ws, &event)
			if err != nil {
				panic(err)
			}

			if event.Event != pusherPing {
				t.Errorf("Expected to get ping event, got %+v", event)
			}
			wg.Done()
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			connected:     true,
			activityTimer: time.NewTimer(0),
			ws:            ws,
		}
		defer client.Disconnect()

		go client.heartbeat()

		wg.Wait()
	})

	t.Run("timerReset doesn't block", func(t *testing.T) {
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
		defer srv.Close()

		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			connected:          true,
			activityTimerReset: make(chan struct{}, 1),
			activityTimer:      time.NewTimer(1024 * time.Hour),
			activityTimeout:    0,
			ws:                 ws,
		}

		go client.heartbeat()
		runtime.Gosched()

		// If there's a bug in the implementation, this will block forever.
		// The test should timeout.
		done := make(chan struct{})
		go func() {
			client.resetActivityTimer()
			close(done)
		}()

		select {
		case <-done:
			// test passed
		case <-time.After(3 * time.Second):
			t.Errorf("Timeout waiting for resetActivityTimer to finish")
		}
	})
}

func TestClientListen(t *testing.T) {
	t.Run("notConnected", func(t *testing.T) {
		client := &Client{
			connected: false,
		}

		client.heartbeat()
	})

	t.Run("receivePing", func(t *testing.T) {
		wg := &sync.WaitGroup{}
		wg.Add(1)
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			websocket.Message.Send(ws, pingPayload)

			var event Event
			err := websocket.JSON.Receive(ws, &event)
			if err != nil {
				panic(err)
			}

			if event.Event != pusherPong {
				t.Errorf("Expected to get pong event, got %+v", event)
			}
			wg.Done()
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			connected: true,
			ws:        ws,
		}
		defer client.Disconnect()

		go client.listen()

		wg.Wait()
	})

	t.Run("receiveEvent", func(t *testing.T) {
		wantData := json.RawMessage(`{"hello":"world"}`)
		wantEvent := Event{
			Event:   "foo",
			Channel: "bar",
			Data:    wantData,
		}
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			websocket.JSON.Send(ws, wantEvent)
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		eventChan := make(chan Event)
		dataChan := make(chan json.RawMessage)
		client := &Client{
			connected: true,
			ws:        ws,
			boundEvents: map[string]boundEventChans{
				wantEvent.Event: {eventChan: struct{}{}},
			},
			subscribedChannels: map[string]internalChannel{
				wantEvent.Channel: &channel{
					boundEvents: map[string]boundDataChans{
						wantEvent.Event: {dataChan: make(chan struct{})},
					},
				},
			},
		}
		defer client.Disconnect()

		wg := &sync.WaitGroup{}
		wg.Add(1)
		go func() {
			if gotEvent := <-eventChan; !reflect.DeepEqual(gotEvent, wantEvent) {
				t.Errorf("Expected to receive event %+v, got %+v", wantEvent, gotEvent)
			}
			if gotData := <-dataChan; !reflect.DeepEqual(gotData, wantData) {
				t.Errorf("Expected to receive data %+v, got %+v", wantData, gotData)
			}
			wg.Done()
		}()

		go client.listen()

		wg.Wait()
	})

	t.Run("receiveError", func(t *testing.T) {
		wantError := EventError{
			Code:    1234,
			Message: "foo",
		}
		errData, err := json.Marshal(wantError)
		if err != nil {
			panic(err)
		}
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			for {
				websocket.JSON.Send(ws, Event{Event: pusherError, Data: errData})
			}
		}))
		defer srv.Close()
		wsURL := strings.Replace(srv.URL, "http", "ws", 1)
		ws, err := websocket.Dial(wsURL, "ws", localOrigin)
		if err != nil {
			panic(err)
		}

		client := &Client{
			connected: true,
			ws:        ws,
			Errors:    make(chan error),
		}
		defer client.Disconnect()

		wg := &sync.WaitGroup{}
		wg.Add(1)
		go func() {
			if gotError := <-client.Errors; !reflect.DeepEqual(gotError, wantError) {
				t.Errorf("Expected to receive event %+v, got %+v", wantError, gotError)
			}
			wg.Done()
		}()

		go client.listen()

		wg.Wait()
	})
}

func TestClientGenerateConnURL(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		wantAppKey := "foo"
		client := &Client{}
		gotURL := client.generateConnURL(wantAppKey)
		if !strings.Contains(gotURL, secureScheme) {
			t.Errorf("Expected connection URL to have secure scheme, got %q", gotURL)
		}
		if !strings.Contains(gotURL, fmt.Sprint(securePort)) {
			t.Errorf("Expected connection URL to have secure port, got %q", gotURL)
		}
		if !strings.Contains(gotURL, defaultHost) {
			t.Errorf("Expected connection URL to have default host, got %q", gotURL)
		}
		if !strings.Contains(gotURL, wantAppKey) {
			t.Errorf("Expected connection URL to have app key, got %q", gotURL)
		}
	})

	t.Run("custom", func(t *testing.T) {
		wantAppKey := "foo"
		client := &Client{
			Insecure: true,
			Cluster:  "bar",
		}
		gotURL := client.generateConnURL(wantAppKey)
		if !strings.Contains(gotURL, insecureScheme) {
			t.Errorf("Expected connection URL to have insecure scheme, got %q", gotURL)
		}
		if !strings.Contains(gotURL, fmt.Sprint(insecurePort)) {
			t.Errorf("Expected connection URL to have insecure port, got %q", gotURL)
		}
		if !strings.Contains(gotURL, "pusher.com") {
			t.Errorf("Expected connection URL to have pusher.com, got %q", gotURL)
		}
		if !strings.Contains(gotURL, client.Cluster) {
			t.Errorf("Expected connection URL to have custom cluster, got %q", gotURL)
		}
		if !strings.Contains(gotURL, wantAppKey) {
			t.Errorf("Expected connection URL to have app key, got %q", gotURL)
		}
	})

	t.Run("override", func(t *testing.T) {
		client := &Client{
			OverrideHost: "foo.bar",
			OverridePort: 1234,
		}

		gotURL := client.generateConnURL("")
		if !strings.Contains(gotURL, client.OverrideHost) {
			t.Errorf("Expected connection URL to have override host, got %q", gotURL)
		}
		if !strings.Contains(gotURL, fmt.Sprint(client.OverridePort)) {
			t.Errorf("Expected connection URL to have override port, got %q", gotURL)
		}
	})
}

func TestClientConnect(t *testing.T) {
	t.Run("pusherError", func(t *testing.T) {
		wantError := EventError{
			Code:    1234,
			Message: "foo",
		}
		errData, err := json.Marshal(wantError)
		if err != nil {
			panic(err)
		}
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			for {
				websocket.JSON.Send(ws, Event{Event: pusherError, Data: errData})
			}
		}))
		defer srv.Close()

		host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			panic(err)
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			panic(err)
		}

		client := &Client{
			Insecure:     true,
			OverrideHost: host,
			OverridePort: portNum,
		}
		defer client.Disconnect()

		connectErr := client.Connect("")
		if !reflect.DeepEqual(connectErr, wantError) {
			t.Errorf("Expected error to deep-equal %+v, got %+v", wantError, connectErr)
		}
	})

	t.Run("connectionEstablished", func(t *testing.T) {
		wantConnData := connectionData{
			SocketID:        "foo",
			ActivityTimeout: 1234,
		}
		connData, err := json.Marshal(wantConnData)
		if err != nil {
			panic(err)
		}
		connDataStr, err := json.Marshal(string(connData))
		if err != nil {
			panic(err)
		}
		srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			for {
				websocket.JSON.Send(ws, Event{Event: pusherConnEstablished, Data: connDataStr})
			}
		}))
		defer srv.Close()

		host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			panic(err)
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			panic(err)
		}

		client := &Client{
			Insecure:     true,
			OverrideHost: host,
			OverridePort: portNum,
		}
		defer client.Disconnect()

		err = client.Connect("")
		if err != nil {
			panic(err)
		}

		if client.connected != true {
			t.Errorf("Expected client connected to be true, got false")
		}
		if client.socketID != wantConnData.SocketID {
			t.Errorf("Expected client socket ID to be %v, got %v", wantConnData.SocketID, client.socketID)
		}
		wantTimeout := time.Duration(wantConnData.ActivityTimeout) * time.Second
		if client.activityTimeout != wantTimeout {
			t.Errorf("Expected client activity timeout to be %v, got %v", wantTimeout, client.activityTimeout)
		}
		if client.activityTimer == nil {
			t.Errorf("Expected client activity timer to be non-nil")
		}
		if client.activityTimerReset == nil {
			t.Errorf("Expected client activity timer reset channel to be non-nil")
		}
		if cap(client.activityTimerReset) != 1 {
			t.Errorf("Expected client activity timer reset channel to have capacity 1, got %v",
				cap(client.activityTimerReset))
		}
		if client.boundEvents == nil {
			t.Errorf("Expected client bound events to be non-nil")
		}
		if client.subscribedChannels == nil {
			t.Errorf("Expected client subscribed channels to be non-nil")
		}
	})
}

func TestReconnection(t *testing.T) {
	t.Run("automaticReconnectOnConnectionLoss", func(t *testing.T) {
		// Setup first server that will intentionally close connection
		connectionCount := 0
		var initialServer *httptest.Server
		var wg sync.WaitGroup
		wg.Add(1)

		initialServer = httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			connectionCount++

			// Send connection established event
			connData, _ := json.Marshal(connectionData{
				SocketID:        fmt.Sprintf("socket-%d", connectionCount),
				ActivityTimeout: 1,
			})
			connDataStr, _ := json.Marshal(string(connData))
			websocket.JSON.Send(ws, Event{Event: pusherConnEstablished, Data: connDataStr})

			// For the first connection, close it immediately after establishing
			if connectionCount == 1 {
				// Wait a moment to ensure client processes connection established
				time.Sleep(100 * time.Millisecond)
				ws.Close()
				wg.Done()
			} else {
				// Keep second connection open
				select {}
			}
		}))
		defer initialServer.Close()

		// Create client with error channel to verify reconnection
		errorChan := make(chan error, 10)
		host, port, _ := getServerHostPort(initialServer)

		client := &Client{
			Insecure:     true,
			OverrideHost: host,
			OverridePort: port,
			Errors:       errorChan,
			// Override reconnect delay for faster test execution
			ReconnectDelay: 100 * time.Millisecond,
		}
		defer client.Disconnect()

		// Connect to server
		err := client.Connect("test-app-key")
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}

		// Wait for first connection to be closed
		wg.Wait()

		// Wait for reconnection to happen (should be quick with reduced delay)
		reconnectDetected := false
		timeout := time.After(5 * time.Second)

		for !reconnectDetected {
			select {
			case err := <-errorChan:
				// Look for reconnection success message
				if strings.Contains(err.Error(), "reconnection successful") {
					reconnectDetected = true
				}
			case <-timeout:
				t.Fatal("Timeout waiting for reconnection")
			}
		}

		// Verify connection count
		if connectionCount != 2 {
			t.Errorf("Expected 2 connections, got %d", connectionCount)
		}

		// Verify client is connected
		if !client.isConnected() {
			t.Error("Client should be connected after reconnect")
		}

		// Verify socket ID has changed (indicating a new connection)
		if !strings.Contains(client.socketID, "socket-2") {
			t.Errorf("Expected new socket ID after reconnect, got %s", client.socketID)
		}
	})

	t.Run("exponentialBackoffOnReconnectFailure", func(t *testing.T) {
		// Create a mock server that always refuses connections after initial setup
		initialConnectionEstablished := false
		failureCount := 0

		server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			if !initialConnectionEstablished {
				// Send connection established event for first connection
				connData, _ := json.Marshal(connectionData{
					SocketID:        "test-socket",
					ActivityTimeout: 1,
				})
				connDataStr, _ := json.Marshal(string(connData))
				websocket.JSON.Send(ws, Event{Event: pusherConnEstablished, Data: connDataStr})
				initialConnectionEstablished = true

				// Wait a moment before closing
				time.Sleep(100 * time.Millisecond)
				ws.Close()
			} else {
				// Count connection attempts but immediately close
				failureCount++
				ws.Close()
			}
		}))
		defer server.Close()

		// Create client with error channel
		errorChan := make(chan error, 20)
		host, port, _ := getServerHostPort(server)

		client := &Client{
			Insecure:     true,
			OverrideHost: host,
			OverridePort: port,
			Errors:       errorChan,
			// Start with very small delay for testing
			ReconnectDelay: 50 * time.Millisecond,
		}
		defer client.Disconnect()

		// Connect to server
		err := client.Connect("test-app-key")
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}

		// Collect errors to analyze reconnection attempts
		var reconnectAttempts []time.Duration
		var lastAttemptTime time.Time

		// Wait for at least 4 reconnection attempts
		timeout := time.After(10 * time.Second)
		startTime := time.Now()

	ErrorLoop:
		for len(reconnectAttempts) < 4 {
			select {
			case err := <-errorChan:
				// Look for reconnection attempt messages
				attemptMatch := regexp.MustCompile(`attempting reconnection after ([0-9]+)(\.[0-9]+)?(ms|s)`)
				matches := attemptMatch.FindStringSubmatch(err.Error())

				if len(matches) > 0 {
					if !lastAttemptTime.IsZero() {
						timeSinceLast := time.Since(lastAttemptTime)
						reconnectAttempts = append(reconnectAttempts, timeSinceLast)
					}
					lastAttemptTime = time.Now()
				}
			case <-timeout:
				break ErrorLoop
			}
		}

		// Verify we got at least a few reconnection attempts
		if len(reconnectAttempts) < 3 {
			t.Fatalf("Expected at least 3 reconnection intervals, got %d", len(reconnectAttempts))
		}

		// Verify exponential backoff (each delay should be ~2x the previous)
		for i := 1; i < len(reconnectAttempts); i++ {
			ratio := float64(reconnectAttempts[i]) / float64(reconnectAttempts[i-1])
			// Allow some wiggle room in timing
			if ratio < 1.5 || ratio > 2.5 {
				t.Errorf("Expected exponential backoff with ratio ~2.0, got %f for attempts %d and %d", ratio, i-1, i)
			}
		}

		// Verify delay doesn't exceed maximum
		totalTestTime := time.Since(startTime)
		if totalTestTime > 20*time.Second {
			t.Errorf("Reconnect delays grew too large, test took %v", totalTestTime)
		}
	})

	t.Run("reconnectsAndRestoresSubscriptions", func(t *testing.T) {
		// Track subscription state across connections
		subscriptionCounts := make(map[string]int)
		var subscribedChannels []string
		var connectionMutex sync.Mutex
		connectionCount := 0

		server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
			connectionMutex.Lock()
			connectionCount++
			connID := connectionCount
			connectionMutex.Unlock()

			// Send connection established event
			connData, _ := json.Marshal(connectionData{
				SocketID:        fmt.Sprintf("socket-%d", connID),
				ActivityTimeout: 1,
			})
			connDataStr, _ := json.Marshal(string(connData))
			websocket.JSON.Send(ws, Event{Event: pusherConnEstablished, Data: connDataStr})

			// Process messages until connection closes
			for {
				var evt Event
				err := websocket.JSON.Receive(ws, &evt)
				if err != nil {
					if err == io.EOF {
						break
					}
					continue
				}

				// Handle subscription requests
				if evt.Event == pusherSubscribe {
					var data struct {
						Channel string `json:"channel"`
					}
					json.Unmarshal(evt.Data, &data)

					connectionMutex.Lock()
					subscriptionCounts[data.Channel]++

					// Track channels for the first connection
					if connID == 1 && !contains(subscribedChannels, data.Channel) {
						subscribedChannels = append(subscribedChannels, data.Channel)
					}
					connectionMutex.Unlock()

					// Send subscription success
					websocket.JSON.Send(ws, Event{
						Event:   pusherInternalSubSucceeded,
						Channel: data.Channel,
					})
				}
			}
		}))
		defer server.Close()

		// Create client
		errorChan := make(chan error, 10)
		host, port, _ := getServerHostPort(server)

		client := &Client{
			Insecure:       true,
			OverrideHost:   host,
			OverridePort:   port,
			Errors:         errorChan,
			ReconnectDelay: 100 * time.Millisecond,
		}
		defer client.Disconnect()

		// Connect to server
		err := client.Connect("test-app-key")
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}

		// Subscribe to multiple channels
		channelNames := []string{"test-channel-1", "test-channel-2", "test-channel-3"}
		for _, name := range channelNames {
			_, err := client.Subscribe(name)
			if err != nil {
				t.Fatalf("Failed to subscribe to channel %s: %v", name, err)
			}
		}

		// Wait a moment to ensure subscriptions are processed
		time.Sleep(200 * time.Millisecond)

		// Force a disconnection by closing the websocket
		client.ws.Close()

		// Wait for reconnection and resubscription
		time.Sleep(2 * time.Second)

		// Verify client reconnected
		if !client.isConnected() {
			t.Error("Client should be connected after reconnect")
		}

		// Verify number of connections
		if connectionCount != 2 {
			t.Errorf("Expected 2 connections, got %d", connectionCount)
		}

		// Verify each channel was subscribed to twice (once per connection)
		for _, channel := range channelNames {
			count, exists := subscriptionCounts[channel]
			if !exists {
				t.Errorf("Channel %s was never subscribed to", channel)
			} else if count != 2 {
				t.Errorf("Expected channel %s to be subscribed 2 times, got %d", channel, count)
			}
		}
	})
}

// Helper functions
func getServerHostPort(server *httptest.Server) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		return "", 0, err
	}
	port, err = strconv.Atoi(portStr)
	return host, port, err
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
