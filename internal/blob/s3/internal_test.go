package s3

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
)

// alreadyOwned only fires when two creators race MakeBucket, which a serial
// test cannot arrange against a real endpoint — so the classification is
// pinned white-box on the error codes S3 defines for the lost race.
func TestAlreadyOwnedClassifiesRaceCodes(t *testing.T) {
	for code, want := range map[string]bool{
		"BucketAlreadyOwnedByYou": true,
		"BucketAlreadyExists":     true,
		"NoSuchBucket":            false,
		"AccessDenied":            false,
	} {
		if got := alreadyOwned(minio.ErrorResponse{Code: code}); got != want {
			t.Errorf("alreadyOwned(%s) = %v, want %v", code, got, want)
		}
	}
	if alreadyOwned(errors.New("not an S3 error")) {
		t.Error("alreadyOwned(non-S3 error) = true, want false")
	}
}
