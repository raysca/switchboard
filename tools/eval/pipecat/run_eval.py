"""`python -m pipecat.evals` with one patch: the Go server speaks binary protobuf
frames (a RTVI JSON message rides in Frame.message.data), the harness speaks bare
RTVI JSON text. Wrap/unwrap at the serializer; everything else is stock pipecat."""

from pipecat.cli.commands.eval import eval_app
import pipecat.evals.client as client
from pipecat.evals.client_transport import EvalClientTransport
from pipecat.evals.serializer import EvalClientSerializer
from pipecat.transports.websocket.rtvi_client import RTVIClientTransport
from pipecat.frames.protobufs import frames_pb2


def _wrap(text):
    f = frames_pb2.Frame()
    f.message.data = text
    return f.SerializeToString()


class GoVoiceSerializer(EvalClientSerializer):
    async def serialize(self, frame):
        out = await super().serialize(frame)
        return _wrap(out) if isinstance(out, str) else out


async def _send_client_ready(self):
    # The stock handshake writes JSON text straight to the socket, bypassing the serializer.
    text = RTVIClientTransport._client_ready_text()
    await self._session.send(_wrap(text))


_stock_on_message = EvalClientTransport._on_message


async def _on_message(self, websocket, message):
    if isinstance(message, bytes):
        f = frames_pb2.Frame()
        f.ParseFromString(message)
        if f.WhichOneof("frame") != "message":
            return  # audio/text/transcription frames: text-mode scenarios ignore them
        message = f.message.data
    await _stock_on_message(self, websocket, message)


def _client_ready_text():
    import pipecat.processors.frameworks.rtvi.models as RTVI

    return RTVI.Message(
        type="client-ready",
        id="client-ready",
        data=RTVI.ClientReadyData(
            version=RTVI.PROTOCOL_VERSION, about=RTVI.AboutClientData(library="pipecat")
        ).model_dump(),
    ).model_dump_json()


RTVIClientTransport._client_ready_text = staticmethod(_client_ready_text)
EvalClientTransport._send_client_ready = _send_client_ready
EvalClientTransport._on_message = _on_message
client.EvalClientSerializer = GoVoiceSerializer
eval_app()
