// Mic capture (16 kHz PCM16 frames) and gapless playback of server audio.

export class Mic {
  private ctx?: AudioContext
  private node?: AudioWorkletNode
  private stream?: MediaStream
  onFrame: (pcm: ArrayBuffer) => void = () => {}
  /** Drop frames while true, e.g. while voc is speaking, so it doesn't hear itself. */
  muted = false

  /** Opens the mic once; later start/stop calls only toggle frame delivery. */
  async open() {
    if (this.ctx) return
    if (!navigator.mediaDevices?.getUserMedia) {
      throw new Error('This page needs a secure origin for the microphone: open it at http://localhost or http://127.0.0.1.')
    }
    try {
      this.stream = await getMic()
      // Firefox can't connect a mic to an AudioContext running at a different
      // sample rate (e.g. a 24 kHz headset vs a 48 kHz context), so match it.
      const rate = this.stream.getAudioTracks()[0]?.getSettings().sampleRate
      try {
        this.ctx = rate ? new AudioContext({ sampleRate: rate }) : new AudioContext()
      } catch {
        this.ctx = new AudioContext()
      }
      await this.ctx.audioWorklet.addModule('/pcm-capture.js')
      const src = this.ctx.createMediaStreamSource(this.stream)
      this.node = new AudioWorkletNode(this.ctx, 'pcm-capture')
      this.node.port.onmessage = (e) => this.live && !this.muted && this.onFrame(e.data as ArrayBuffer)
      src.connect(this.node)
    } catch (err) {
      this.stream?.getTracks().forEach((t) => t.stop())
      void this.ctx?.close()
      this.ctx = this.stream = this.node = undefined // let the next press retry
      throw new Error(explainMicError(err))
    }
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

/** Asks for the mic with speech-friendly processing, falling back to plain
 * {audio: true} when a browser rejects the constraints. */
async function getMic(): Promise<MediaStream> {
  try {
    return await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    })
  } catch (err) {
    const name = (err as DOMException)?.name
    if (name === 'NotFoundError' || name === 'OverconstrainedError' || name === 'NotReadableError') {
      return await navigator.mediaDevices.getUserMedia({ audio: true })
    }
    throw err
  }
}

/** Turns browser mic errors into something a person can act on. */
function explainMicError(err: unknown): string {
  const e = err as DOMException
  const firefox = navigator.userAgent.includes('Firefox')
  const mac = navigator.userAgent.includes('Mac')
  const osHint = mac
    ? ` On macOS, check System Settings → Privacy & Security → Microphone and turn on ${firefox ? 'Firefox' : 'your browser'}, then restart the browser.`
    : ''
  switch (e?.name) {
    case 'NotAllowedError':
    case 'SecurityError':
      return `Microphone permission was denied. Allow it from the icon in the address bar and try again.${osHint}`
    case 'NotFoundError':
      // Firefox also reports this when the OS blocks the browser from the mic.
      return `No usable microphone was found (${e.message}).${osHint} Also check that an input device is selected in Sound settings.`
    case 'NotReadableError':
    case 'AbortError':
      return `The microphone is busy or failed to start; close other apps using it (Zoom, Teams) and try again. (${e.message})`
    case 'NotSupportedError':
      return `This browser couldn't connect the microphone to audio processing: ${e.message}`
  }
  return e?.message || String(err)
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
