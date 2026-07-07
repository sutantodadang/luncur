package gpucloud

// Compile-time check that *VastAI satisfies Provider; a real assertion also
// lives in provider.go, this one just keeps the test package honest.
var _ Provider = (*VastAI)(nil)
