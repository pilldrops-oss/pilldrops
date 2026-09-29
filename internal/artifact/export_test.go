package artifact

import "time"

// S3SignAtForTest pins the signing clock and body hash so the known-answer
// test can reproduce AWS's published example signature. Test-only.
func S3SignAtForTest(s *S3, at time.Time, payloadHash string) {
	s.clock = func() time.Time { return at }
	s.payload = payloadHash
}
