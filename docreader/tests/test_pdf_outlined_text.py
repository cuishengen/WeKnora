"""Text converted to vector outlines still needs the existing image/OCR route."""
import unittest

from docreader.main import DocReaderServicer
from docreader.parser.pdf_parser import PDFParser
from docreader.proto.docreader_pb2 import ReadRequest
from docreader.tests.test_docreader_stream import _NoopPreprocessRunner


def _vector_pdf(painted=True):
    drawing = b"0 g 30 40 8 25 re f 45 40 8 25 re f" if painted else b""
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 120 160] /Resources << >> /Contents 4 0 R >>",
        b"<< /Length " + str(len(drawing)).encode() + b" >>\nstream\n" + drawing + b"\nendstream",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = [0]
    for number, body in enumerate(objects, 1):
        offsets.append(len(out))
        out.extend(str(number).encode() + b" 0 obj\n" + body + b"\nendobj\n")
    start = len(out)
    out.extend(b"xref\n0 5\n0000000000 65535 f \n")
    for offset in offsets[1:]:
        out.extend(f"{offset:010d} 00000 n \n".encode())
    out.extend(f"trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n{start}\n%%EOF\n".encode())
    return bytes(out)


class OutlinedTextPDFTest(unittest.TestCase):
    def test_vector_only_page_uses_lazy_scanned_image_route(self):
        stream = PDFParser(file_name="outlined.pdf").parse_stream(_vector_pdf())
        try:
            self.assertEqual(stream.document.metadata["scanned_page_count"], 1)
            self.assertEqual(stream.image_count, 1)
            self.assertIn("outlined_page_1.jpg", stream.document.content)
            self.assertTrue(next(iter(stream.images)).image_data.startswith(b"\xff\xd8"))
        finally:
            stream.close()

    def test_blank_pdf_is_explicit_content_rejection_in_both_rpc_modes(self):
        service = DocReaderServicer(preprocess_runner=_NoopPreprocessRunner())
        try:
            for mode in ("unary", "stream"):
                request = ReadRequest(file_content=_vector_pdf(False), file_name="blank.pdf", file_type="pdf")
                response = service.Read(request, None) if mode == "unary" else next(service.ReadStream(request, None)).meta
                self.assertTrue(response.error)
                self.assertEqual(response.metadata["error_kind"], "document_content")
                self.assertEqual(response.metadata["error_code"], "pdf_empty_content")
        finally:
            service.close()
