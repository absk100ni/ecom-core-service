package contact

import "testing"

func TestEmailValidation(t *testing.T) {
	valid := []string{"test@example.com", "user.name+tag@domain.co.in", "hello@sub.domain.org"}
	invalid := []string{"", "notanemail", "@nodomain", "missing@.com", "spaces in@email.com"}

	for _, e := range valid {
		if !emailRegex.MatchString(e) {
			t.Errorf("Expected %q to be valid", e)
		}
	}
	for _, e := range invalid {
		if emailRegex.MatchString(e) {
			t.Errorf("Expected %q to be invalid", e)
		}
	}
}
