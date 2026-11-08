// Mic capture (16 kHz PCM16 frames) and gapless playback of server audio.

export class Mic {
  private ctx?: AudioContext
  private node?: AudioWorkletNode
  private stream?: MediaStream
  onFrame: (pcm: ArrayBuffer) => void = () => {}

  /** Opens the mic once; later start/stop calls only toggle frame delivery. */
  async open() {
    if (this.ctx) return
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    })
    this.ctx = new AudioContext()
    await this.ctx.audioWorklet.addModule('/pcm-capture.js')
    const src = this.ctx.createMediaStreamSource(this.stream)
    this.node = new AudioWorkletNode(this.ctx, 'pcm-capture')
    this.node.port.onmessage = (e) => this.live && this.onFrame(e.data as ArrayBuffer)
    src.connect(this.node)
  }

  private live = false
  async start() {
    await this.open()
    await this.ctx!.resume()
    this.live = true
  }
  stop() {
    this.live = false
  }
}

/** Plays downstream frames: [u32 turn][u32 sampleRate][PCM16 LE mono]. */
export class Player {
  private ctx = new AudioContext()
  private next = 0
  private sources = new Set<AudioBufferSourceNode>()
  onPlaying?: (playing: boolean) => void

  static turnOf(frame: ArrayBuffer) {
    return new DataView(frame).getUint32(0, true)
  }

  /** Schedules a frame right after the previous one. Returns ms until it starts. */
  play(frame: ArrayBuffer): number {
    const rate = new DataView(frame).getUint32(4, true)
    const pcm = new Int16Array(frame, 8)
    if (pcm.length === 0) return 0
    const buf = this.ctx.createBuffer(1, pcm.length, rate)
    const ch = buf.getChannelData(0)
    for (let i = 0; i < pcm.length; i++) ch[i] = pcm[i] / 32768
    const src = this.ctx.createBufferSource()
    src.buffer = buf
    src.connect(this.ctx.destination)
    void this.ctx.resume()
    const at = Math.max(this.ctx.currentTime + 0.02, this.next)
    src.start(at)
    this.next = at + buf.duration
    this.sources.add(src)
    this.onPlaying?.(true)
    src.onended = () => {
      this.sources.delete(src)
      if (this.sources.size === 0) this.onPlaying?.(false)
    }
    return (at - this.ctx.currentTime) * 1000
  }

  /** Stops everything that is playing or queued. */
  stop() {
    for (const s of this.sources) {
      s.onended = null
      s.stop()
    }
    this.sources.clear()
    this.next = 0
    this.onPlaying?.(false)
  }
}
