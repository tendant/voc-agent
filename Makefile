.PHONY: dev server web build models test spike clean offline-check rehearse demo-reset

models:            ## download speech models into ./models
	./scripts/fetch-models.sh

dev: models web/node_modules   ## Go server on :8080 + Vite on :5173 (open http://localhost:5173)
	@trap 'kill 0' EXIT; \
	  go run ./cmd/voc -static= & \
	  (cd web && npm run dev) & \
	  wait

server:
	go run ./cmd/voc

web/node_modules:
	cd web && npm install

build: web/node_modules        ## single binary serving web/dist on :8080
	cd web && npm run build
	go build -o bin/voc ./cmd/voc

test:
	go test ./...

spike:             ## sherpa-onnx TTS→VAD→ASR round trip with timings
	go run ./cmd/voicespike

offline-check: models   ## dress rehearsal by voice + proof that only loopback is used
	./scripts/offline-check.sh

rehearse:          ## run the demo script by voice against a running server (make dev)
	go run ./cmd/rehearse -url ws://127.0.0.1:8080/ws

demo-reset:        ## forget conversation, reminders and audit log (keeps notes)
	rm -f voc.db voc.db-wal voc.db-shm

clean:
	rm -rf bin web/dist
