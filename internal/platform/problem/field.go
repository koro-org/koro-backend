package problem

// Field returns ErrValidation (422 validation_failed) pointing at one input
// location ("body.title", "query.org_id"). msg never echoes the input value.
func Field(location, msg string) error {
	return ErrValidation.WithFields(FieldError{Location: location, Message: msg})
}
