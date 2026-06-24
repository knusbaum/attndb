"""One-time export of a single-vector embedder to ONNX with pooling + L2
normalization baked into the graph, so the Go side just runs it and reads one
vector. Mirrors the ColBERT export.

Run inside the isolated export env: `pipenv run python scripts/export_single.py`
"""

import os
import torch
import torch.nn as nn
from sentence_transformers import SentenceTransformer

NAME = "Alibaba-NLP/gte-modernbert-base"
OUT = "models/single"


class Wrap(nn.Module):
    def __init__(self, st):
        super().__init__()
        self.st = st

    def forward(self, input_ids, attention_mask):
        feats = {"input_ids": input_ids, "attention_mask": attention_mask}
        for module in self.st:  # Transformer -> Pooling -> Normalize
            feats = module(feats)
        return feats["sentence_embedding"]


def main():
    # Force CPU: on Apple Silicon PyTorch defaults to MPS, which causes device
    # conflicts in torch.onnx.export's FakeTensor propagation.
    st = SentenceTransformer(NAME, device="cpu")
    # Cast to float32: the HuggingFace model is stored in float16 but the Go
    # runtime expects float32 tensors.
    st = st.float()
    st.eval()
    os.makedirs(OUT, exist_ok=True)

    ids = torch.tensor([[0, 1, 2, 3]], dtype=torch.long)
    mask = torch.ones_like(ids)
    torch.onnx.export(
        Wrap(st),
        (ids, mask),
        OUT + "/model.onnx",
        input_names=["input_ids", "attention_mask"],
        output_names=["sentence_embedding"],  # avoid collision with ModernBERT's internal "embedding"
        dynamic_axes={
            "input_ids": {0: "batch", 1: "seq"},
            "attention_mask": {0: "batch", 1: "seq"},
            "sentence_embedding": {0: "batch", 1: "dim"},
        },
        opset_version=20,
    )
    st.tokenizer.save_pretrained(OUT)
    print("EXPORTED", NAME, "dim", st.get_sentence_embedding_dimension())


if __name__ == "__main__":
    main()
