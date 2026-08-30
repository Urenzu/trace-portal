package proxy

import "context"

func withExchange(ctx context.Context, ex *exchange) context.Context {
	return context.WithValue(ctx, ctxKey{}, ex)
}

func exchangeFrom(ctx context.Context) (*exchange, bool) {
	ex, ok := ctx.Value(ctxKey{}).(*exchange)
	return ex, ok
}
