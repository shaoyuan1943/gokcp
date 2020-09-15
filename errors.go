package gokcp

import "errors"

var (
	ErrDataInvalid      = errors.New("data invalid")
	ErrDataLenInvalid   = errors.New("data length invalid")
	ErrDifferenceConvID = errors.New("difference conv ID")
	ErrNoReadableData   = errors.New("no readable data")
	ErrNoEnoughSpace    = errors.New("no enough space to add data")
)
