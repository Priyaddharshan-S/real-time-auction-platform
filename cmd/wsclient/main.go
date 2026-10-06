// Tiny test client: go run ./cmd/wsclient <access-token> <auction-id> [host:port]
// Prints every realtime message for that auction.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/coder/websocket"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: go run ./cmd/wsclient <access-token> <auction-id> [host:port]")
		os.Exit(1)
	}
	token, auctionID, host := os.Args[1], os.Args[2], "localhost:8080"
	if len(os.Args) > 3 {
		host = os.Args[3]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, _, err := websocket.Dial(ctx, "ws://"+host+"/ws?token="+token, nil)
	if err != nil {
		fmt.Println("connect failed:", err)
		os.Exit(1)
	}
	defer conn.CloseNow()

	sub := fmt.Sprintf(`{"action":"subscribe","auction_id":%q}`, auctionID)
	if err := conn.Write(ctx, websocket.MessageText, []byte(sub)); err != nil {
		fmt.Println("write failed:", err)
		os.Exit(1)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Println("closed:", err)
			return
		}
		fmt.Println(string(data))
	}
}
