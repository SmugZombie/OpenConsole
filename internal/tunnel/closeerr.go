package tunnel

import (
	"errors"

	"github.com/coder/websocket"
)

// asCloseError is errors.As specialised to websocket.CloseError, kept apart so
// websocket.go reads without the type-assertion noise.
func asCloseError(err error, target *websocket.CloseError) bool {
	return errors.As(err, target)
}
