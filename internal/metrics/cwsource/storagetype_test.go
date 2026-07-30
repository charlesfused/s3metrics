package cwsource

import "testing"

func TestIsOverhead(t *testing.T) {
	tests := []struct {
		storageType string
		want        bool
	}{
		{"StandardStorage", false},
		{"GlacierStorage", false},
		{"DeepArchiveStorage", false},
		{"GlacierStagingStorage", false},
		{"GlacierObjectOverhead", true},
		{"GlacierS3ObjectOverhead", true},
		{"DeepArchiveObjectOverhead", true},
		{"DeepArchiveS3ObjectOverhead", true},
		{"StandardIASizeOverhead", true},
		{"OneZoneIASizeOverhead", true},
		{"GlacierInstantRetrievalSizeOverhead", true},
		// A storage type AWS has not invented yet must count as real data.
		{"FutureClassStorage", false},
	}
	for _, tt := range tests {
		t.Run(tt.storageType, func(t *testing.T) {
			if got := IsOverhead(tt.storageType); got != tt.want {
				t.Errorf("IsOverhead(%q) = %v, want %v", tt.storageType, got, tt.want)
			}
		})
	}
}

func TestClassName(t *testing.T) {
	tests := []struct {
		storageType string
		want        string
	}{
		{"StandardStorage", "STANDARD"},
		{"StandardIAStorage", "STANDARD_IA"},
		{"OneZoneIAStorage", "ONEZONE_IA"},
		{"ReducedRedundancyStorage", "REDUCED_REDUNDANCY"},
		{"GlacierStorage", "GLACIER"},
		{"GlacierInstantRetrievalStorage", "GLACIER_IR"},
		{"DeepArchiveStorage", "DEEP_ARCHIVE"},
		{"ExpressOneZoneStorage", "EXPRESS_ONEZONE"},
		{"IntelligentTieringFAStorage", "INTELLIGENT_TIERING_FA"},
		{"IntelligentTieringDAAStorage", "INTELLIGENT_TIERING_DAA"},
		{"GlacierObjectOverhead", "GLACIER_OBJECT_OVERHEAD"},
		{"StandardIASizeOverhead", "STANDARD_IA_SIZE_OVERHEAD"},
		// Unknown types pass through by raw name rather than being dropped:
		// silently discarding bytes is worse than an unfamiliar label.
		{"SomeNewStorage", "SomeNewStorage"},
	}
	for _, tt := range tests {
		t.Run(tt.storageType, func(t *testing.T) {
			if got := ClassName(tt.storageType); got != tt.want {
				t.Errorf("ClassName(%q) = %q, want %q", tt.storageType, got, tt.want)
			}
		})
	}
}
