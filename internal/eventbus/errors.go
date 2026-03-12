package eventbus

import "errors"

// ErrBusClosed is returned when publishing to a closed bus.
var ErrBusClosed = errors.New("eventbus: bus is closed")
