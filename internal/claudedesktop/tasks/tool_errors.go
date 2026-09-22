package tasks

import "errors"

// IsValidateInputRejection reports whether err is a native validateInput
// rejection of the owned tool family (TaskOutput/TaskStop: missing id, not
// found, not running). The generic tool wrapper renders these as
// <tool_use_error>message</tool_use_error> and counts them as
// tengu_feature_sad tool_validate_input_rejected; errors thrown inside call
// (state, cancellation, runtime failures) are not rejections.
func IsValidateInputRejection(err error) bool {
	var rejected validationError
	return errors.As(err, &rejected)
}

// NewValidateInputRejection builds a validateInput rejection with the native
// message text (rendered as <tool_use_error>message</tool_use_error>).
func NewValidateInputRejection(message string) error {
	return validationError{message}
}
