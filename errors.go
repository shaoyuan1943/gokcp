package gokcp

import "errors"

var (
	ErrDataInvalid      = errors.New("data invalid")
	ErrDataTooLong      = errors.New("the data too long")
	ErrDifferenceConvID = errors.New("difference conv ID")
	ErrNoReadableData   = errors.New("no readable data")
	ErrNoEnoughSpace    = errors.New("no enough space to add data")
)
