//go:build !linux

package boot

import "context"

func reapLoop(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
