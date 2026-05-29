//go:build !allow_raw_ffmpeg_args

package ffmpeg

// rawArgsEnabled returns false when the allow_raw_ffmpeg_args build tag is NOT set.
func rawArgsEnabled() bool {
	return false
}
