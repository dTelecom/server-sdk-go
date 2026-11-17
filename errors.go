package lksdk

import "errors"

var (
	ErrURLNotProvided           = errors.New("URL was not provided")
	ErrConnectionTimeout        = errors.New("could not connect after timeout")
	ErrTrackPublishTimeout      = errors.New("timed out publishing track")
	ErrCannotDetermineMime      = errors.New("cannot determine mimetype from file extension")
	ErrUnsupportedFileType      = errors.New("ReaderSampleProvider does not support this mime type")
	ErrUnsupportedSimulcastKind = errors.New("simulcast is only supported for video")
	ErrInvalidSimulcastTrack    = errors.New("simulcast track was not initiated correctly")
	ErrCannotFindTrack          = errors.New("could not find the track")
	ErrInvalidParameter         = errors.New("invalid parameter")
	ErrCannotConnectSignal      = errors.New("could not establish signal connection")
	// ErrPublishNotPermitted: the participant's token does not allow publishing this track
	// (canPublish false, or the source is not in canPublishSources). The node would not answer.
	ErrPublishNotPermitted = errors.New("not permitted to publish this track")
)
