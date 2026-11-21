package storage

import (
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	k, err := ParseKey(strings.NewReader("# made by the admin\nexport AWS_ACCESS_KEY_ID=\"HOSTKEY01\"\nAWS_SECRET_ACCESS_KEY='s3cr3t/+abc'\nAWS_DEFAULT_REGION=default\n"))
	if err != nil || k.AccessKeyID != "HOSTKEY01" || k.SecretAccessKey != "s3cr3t/+abc" {
		t.Fatalf("got %q / %q, %v", k.AccessKeyID, k.SecretAccessKey, err)
	}
	for _, bad := range []string{
		"AWS_ACCESS_KEY_ID=HOSTKEY01\n",
		"AWS_SECRET_ACCESS_KEY=only-the-secret\n",
		"AWS_ACCESS_KEY_ID=HOSTKEY01\nAWS_SECRET_ACCESS_KEY=has a-space-in-it\n",
	} {
		_, err := ParseKey(strings.NewReader(bad))
		if err == nil {
			t.Errorf("accepted %q", bad)
			continue
		}
		if strings.Contains(err.Error(), "only-the-secret") || strings.Contains(err.Error(), "a-space-in-it") {
			t.Errorf("the error shows the secret: %v", err)
		}
	}
}
