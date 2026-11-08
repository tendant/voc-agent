package voice

// Resample converts mono audio between rates with linear interpolation.
// Good enough for feeding TTS output to ASR in tests and for 22.05→16 kHz
// conversions; the browser does its own resampling for mic input.
func Resample(in []float32, from, to int) []float32 {
	if from == to || len(in) == 0 {
		return in
	}
	n := int(int64(len(in)) * int64(to) / int64(from))
	out := make([]float32, n)
	step := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * step
		j := int(pos)
		if j+1 >= len(in) {
			out[i] = in[len(in)-1]
			continue
		}
		f := float32(pos - float64(j))
		out[i] = in[j]*(1-f) + in[j+1]*f
	}
	return out
}
