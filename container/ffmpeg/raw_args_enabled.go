//go:build allow_raw_ffmpeg_args

package ffmpeg

// rawArgsEnabled returns true when the allow_raw_ffmpeg_args build tag is set.
func rawArgsEnabled() bool {
	return true
}
