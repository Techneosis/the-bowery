//go:build !linux && !darwin

package offices

import (
	"context"
	"errors"
	"time"
)

// dirWatch has no native implementation off Linux and macOS: the directory inbox
// polls instead.
type dirWatch struct{}

func newDirWatch(string) (*dirWatch, error) {
	return nil, errors.New("no directory watch on this platform")
}

func (w *dirWatch) wait(context.Context, time.Time) (bool, error) { return false, nil }

func (w *dirWatch) close() {}
