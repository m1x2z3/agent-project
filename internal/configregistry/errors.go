package configregistry

import "errors"

var (
	ErrVersionExists            = errors.New("version_exists")
	ErrVersionNotFound          = errors.New("version_not_found")
	ErrNoActiveVersion          = errors.New("no_active_version")
	ErrInvalidSnapshot          = errors.New("invalid_snapshot")
	ErrRunAlreadyBound          = errors.New("run_already_bound")
	ErrRunNotFound              = errors.New("run_not_found")
	ErrVersionReferenceNotFound = errors.New("version_reference_not_found")
)
