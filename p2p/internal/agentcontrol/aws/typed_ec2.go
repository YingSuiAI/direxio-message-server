package aws

import "crypto/sha256"

// These markers identify the generic typed EC2 provider boundary.  They are
// provider implementation details, not a public provisioning action or
// workload profile.
const (
	EC2ServiceProfile     = "typed-ec2"
	ec2TemplateVersion    = "ec2-typed-v1"
	ec2LatestAMIParameter = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
)

// CanonicalJSONHash is retained for generic provider/readback tests and
// idempotency calculations; it does not encode any workload profile.
func CanonicalJSONHash(value []byte) string {
	sum := sha256.Sum256(value)
	const hex = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, b := range sum {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0xf]
	}
	return string(out)
}
