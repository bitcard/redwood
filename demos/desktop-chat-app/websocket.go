package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/context"

	"redwood.dev"
)

const (
	writeWait  = 10 * time.Second    // Time allowed to write a message to the peer.
	pongWait   = 10 * time.Second    // Time allowed to read the next pong message from the peer.
	pingPeriod = (pongWait * 9) / 10 // Send pings to peer with this period. Must be less than pongWait.
)

var (
	newline  = []byte{'\n'}
	space    = []byte{' '}
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
)

type Client struct {
	sub  redwood.ReadableSubscription
	conn *websocket.Conn
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	var (
		chKill = make(chan struct{})
		chSub  = make(chan redwood.SubscriptionMsg)
	)

	go func() {
		// defer c.sub.Close()
		defer close(chSub)
		for {
			msg, err := c.sub.Read()
			if err != nil {
				fmt.Println("error", err)
				return
			}
			select {
			case chSub <- *msg:
			case <-app.chLoggedOut:
				return
			case <-chKill:
				return
			}
		}
	}()

	go func() {
		defer close(chKill)
		for {
			select {
			case <-app.chLoggedOut:
				return

			case msg, ok := <-chSub:
				if !ok {
					return
				}
				c.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if !ok {
					c.conn.WriteMessage(websocket.CloseMessage, []byte{})
					return
				}

				err := func() error {
					w, err := c.conn.NextWriter(websocket.TextMessage)
					if err != nil {
						fmt.Println("error obtaining next writer:", err)
						return err
					}
					defer w.Close()

					bs, err := json.Marshal(msg)
					if err != nil {
						fmt.Println("error marshaling message json:", err)
						return err
					}

					_, err = w.Write([]byte(string(bs) + "\n"))
					if err != nil {
						fmt.Println("error writing to websocket client:", err)
						return err
					}
					return nil
				}()
				if err != nil {
					return
				}

			case <-ticker.C:
				c.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					fmt.Println("error pinging websocket client:", err)
					return
				}
			}
		}
	}()

	<-chKill
}

// serveWs handles websocket requests from the peer.
func serveWs(w http.ResponseWriter, r *http.Request) {
	stateURI := r.URL.Query().Get("state_uri")

	sub, err := app.host.Subscribe(context.Background(), stateURI, redwood.SubscriptionType_States|redwood.SubscriptionType_Txs, nil, nil)
	if err != nil {
		log.Println(err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	client := &Client{sub: sub, conn: conn}
	client.writePump()
}
