package httpadapter_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/gezibash/arc/sdk/provider"
)

func TestHTTPContextContract(t *testing.T) {
	for _, expire := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if expire {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
		}
		cancel()
		ran := false
		handler := httpadapter.New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
		_, err := handler.HandleRequest(ctx, provider.Request{Message: "{}"})
		if !errors.Is(err, ctx.Err()) || ran {
			t.Errorf("HTTP error = %v, handler ran = %v; want %v before dispatch", err, ran, ctx.Err())
		}
	}
	t.Run("during_dispatch", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		handler := httpadapter.New(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			want, _ := ctx.Deadline()
			got, ok := r.Context().Deadline()
			if !ok || !got.Equal(want) {
				t.Error("HTTP handler lost its caller deadline")
			}
			cancel()
			<-r.Context().Done()
		}))
		reply, err := handler.HandleRequest(ctx, provider.Request{Message: "{}"})
		if !errors.Is(err, context.Canceled) || reply != "" {
			t.Fatalf("canceled HTTP handler returned %q, %v", reply, err)
		}
	})
}
