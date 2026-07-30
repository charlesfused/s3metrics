// Package cwsource collects bucket metrics from CloudWatch's free daily S3
// storage metrics.
package cwsource

import "strings"

// classNames maps CloudWatch StorageType dimension values to the canonical
// storage-class names S3 itself uses, so a report reads the same whichever mode
// produced it.
var classNames = map[string]string{
	"StandardStorage":                     "STANDARD",
	"StandardIAStorage":                   "STANDARD_IA",
	"OneZoneIAStorage":                    "ONEZONE_IA",
	"ReducedRedundancyStorage":            "REDUCED_REDUNDANCY",
	"GlacierStorage":                      "GLACIER",
	"GlacierStagingStorage":               "GLACIER_STAGING",
	"GlacierInstantRetrievalStorage":      "GLACIER_IR",
	"DeepArchiveStorage":                  "DEEP_ARCHIVE",
	"DeepArchiveStagingStorage":           "DEEP_ARCHIVE_STAGING",
	"ExpressOneZoneStorage":               "EXPRESS_ONEZONE",
	"IntelligentTieringFAStorage":         "INTELLIGENT_TIERING_FA",
	"IntelligentTieringIAStorage":         "INTELLIGENT_TIERING_IA",
	"IntelligentTieringAAStorage":         "INTELLIGENT_TIERING_AA",
	"IntelligentTieringAIAStorage":        "INTELLIGENT_TIERING_AIA",
	"IntelligentTieringDAAStorage":        "INTELLIGENT_TIERING_DAA",
	"GlacierObjectOverhead":               "GLACIER_OBJECT_OVERHEAD",
	"GlacierS3ObjectOverhead":             "GLACIER_S3_OBJECT_OVERHEAD",
	"DeepArchiveObjectOverhead":           "DEEP_ARCHIVE_OBJECT_OVERHEAD",
	"DeepArchiveS3ObjectOverhead":         "DEEP_ARCHIVE_S3_OBJECT_OVERHEAD",
	"StandardIASizeOverhead":              "STANDARD_IA_SIZE_OVERHEAD",
	"OneZoneIASizeOverhead":               "ONEZONE_IA_SIZE_OVERHEAD",
	"GlacierInstantRetrievalSizeOverhead": "GLACIER_IR_SIZE_OVERHEAD",
}

// overheadSuffixes identify StorageType values that measure per-object
// bookkeeping rather than object data — Glacier's ~32KB of metadata per object,
// for instance.
var overheadSuffixes = []string{"ObjectOverhead", "SizeOverhead"}

// IsOverhead reports whether a StorageType measures overhead rather than object
// data.
//
// This is a suffix rule rather than a fixed list on purpose. AWS adds storage
// classes; a closed list would silently misclassify each new one, and the
// failure mode of guessing wrong matters: an unknown class treated as overhead
// vanishes from the total, while one treated as real data merely carries an
// unfamiliar label.
func IsOverhead(storageType string) bool {
	for _, suffix := range overheadSuffixes {
		if strings.HasSuffix(storageType, suffix) {
			return true
		}
	}
	return false
}

// ClassName maps a StorageType to a canonical class name, falling back to the
// raw value for anything unrecognised.
func ClassName(storageType string) string {
	if name, ok := classNames[storageType]; ok {
		return name
	}
	return storageType
}
