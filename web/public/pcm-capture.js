// AudioWorklet: downsample the mic to 16 kHz mono PCM16 and post 20 ms frames.
// Averages the input samples falling in each output slot, which acts as a
// crude low-pass filter and is good enough for speech recognition.
const OUT_RATE = 16000
const FRAME = 320 // 20 ms at 16 kHz

class PcmCapture extends AudioWorkletProcessor {
  constructor() {
    super()
    this.ratio = sampleRate / OUT_RATE
    this.pos = 0 // fractional position within the current output slot
    this.acc = 0
    this.n = 0
    this.frame = new Int16Array(FRAME)
    this.i = 0
  }

  process(inputs) {
    const ch = inputs[0] && inputs[0][0]
    if (!ch) return true
    for (let k = 0; k < ch.length; k++) {
      this.acc += ch[k]
      this.n++
      this.pos += 1
      if (this.pos >= this.ratio) {
        this.pos -= this.ratio
        const v = Math.max(-1, Math.min(1, this.acc / this.n))
        this.acc = 0
        this.n = 0
        this.frame[this.i++] = v < 0 ? v * 0x8000 : v * 0x7fff
        if (this.i === FRAME) {
          this.port.postMessage(this.frame.buffer, [this.frame.buffer])
          this.frame = new Int16Array(FRAME)
          this.i = 0
        }
      }
    }
    return true
  }
}

registerProcessor('pcm-capture', PcmCapture)
