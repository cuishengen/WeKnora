import io
import os
import unittest
from dataclasses import replace
from types import SimpleNamespace
from unittest.mock import patch

from PIL import Image
import pypdfium2 as pdfium
import pypdfium2.raw as pdfium_raw

import docreader.parser.pdf_parser as pdf_parser
from docreader.main import DocReaderServicer
from docreader.proto.docreader_pb2 import ReadConfig, ReadRequest


def _scanned_pdf(page_count=2):
    buffer = io.BytesIO()
    pages = [Image.new("RGB", (64, 64), color) for color in ("white", "black")]
    sequence = (pages * ((page_count // len(pages)) + 1))[:page_count]
    sequence[0].save(
        buffer,
        format="PDF",
        save_all=True,
        append_images=sequence[1:],
    )
    return buffer.getvalue()


class _NoopPreprocessRunner:
    def __init__(self):
        self.limits = SimpleNamespace(max_workers=1)

    def run(self, *args, **kwargs):
        return None

    def close(self):
        pass


class ReadStreamPDFTest(unittest.TestCase):
    def test_malformed_pdf_preserves_typed_rejection_in_both_rpc_modes(self):
        content = b"%PDF-1.7\ninvalid isolated candidate structure\n%%EOF\n"
        service = DocReaderServicer(preprocess_runner=_NoopPreprocessRunner())
        try:
            for mode in ("unary", "stream"):
                with self.subTest(mode=mode):
                    request = ReadRequest(
                        file_content=content,
                        file_name="broken.pdf",
                        file_type="pdf",
                    )
                    if mode == "unary":
                        response = service.Read(request, None)
                    else:
                        frames = list(service.ReadStream(request, None))
                        self.assertEqual(len(frames), 1)
                        response = frames[0].meta
                    self.assertTrue(response.error)
                    self.assertEqual(dict(response.metadata), {
                        "error_kind": "document_content",
                        "error_source": "pdfium",
                        "error_code": "pdf_invalid_format",
                    })
        finally:
            service.close()

    def test_typed_pdfium_codes_do_not_depend_on_error_text(self):
        for code, reason in (
            (pdfium_raw.FPDF_ERR_FORMAT, "pdf_invalid_format"),
            (pdfium_raw.FPDF_ERR_PASSWORD, "pdf_password_required"),
            (pdfium_raw.FPDF_ERR_SECURITY, "pdf_unsupported_security"),
        ):
            with self.subTest(code=code):
                error = pdfium.PdfiumError("arbitrary localized detail", err_code=code)
                metadata = pdf_parser.pdfium_error_metadata(error)
                self.assertEqual(metadata["error_kind"], "document_content")
                self.assertEqual(metadata["error_code"], reason)

    def test_operational_failures_are_not_document_rejections(self):
        same_text = "Failed to load document (PDFium: Data format error)."
        failures = [
            pdfium.PdfiumError(same_text, err_code=code)
            for code in (None, pdfium_raw.FPDF_ERR_UNKNOWN, pdfium_raw.FPDF_ERR_FILE,
                         pdfium_raw.FPDF_ERR_PAGE, -1)
        ]
        failures += [TimeoutError(same_text), OSError(same_text),
                     PermissionError(same_text), MemoryError(same_text),
                     RuntimeError(same_text)]
        service = DocReaderServicer(preprocess_runner=_NoopPreprocessRunner())
        try:
            for error in failures:
                with self.subTest(error=type(error).__name__, code=getattr(error, "err_code", None)):
                    self.assertEqual(pdf_parser.pdfium_error_metadata(error), {})
                    request = ReadRequest(file_content=b"doc", file_name="file.pdf", file_type="pdf")
                    with patch.object(service.parser, "parse_file", side_effect=error):
                        self.assertEqual(dict(service.Read(request, None).metadata), {})
                    with patch.object(service.parser, "parse_file_stream", side_effect=error):
                        frames = list(service.ReadStream(request, None))
                        self.assertEqual(len(frames), 1)
                        self.assertEqual(dict(frames[0].meta.metadata), {})
        finally:
            service.close()

    def test_metadata_precedes_lazy_scanned_page_rendering(self):
        config = ReadConfig()
        config.parser_engine_overrides["pdf_force_scanned"] = "true"
        request = ReadRequest(
            file_content=_scanned_pdf(2),
            file_name="scan.pdf",
            file_type="pdf",
            config=config,
        )
        service = DocReaderServicer(preprocess_runner=_NoopPreprocessRunner())
        real_render = pdf_parser._render_page_to_jpeg
        stream = None

        try:
            with patch.object(
                pdf_parser,
                "CONFIG",
                replace(pdf_parser.CONFIG, pdf_render_parallelism=1),
            ):
                with patch.object(
                    pdf_parser,
                    "_render_page_to_jpeg",
                    wraps=real_render,
                ) as render:
                    stream = service.ReadStream(request, None)
                    meta = next(stream).meta
                    self.assertEqual(meta.image_count, 2)
                    self.assertEqual(render.call_count, 0)

                    first = next(stream).image
                    self.assertEqual(first.filename, "scan_page_1.jpg")
                    self.assertTrue(first.image_data.startswith(b"\xff\xd8"))
                    self.assertEqual(render.call_count, 1)

                    second = next(stream).image
                    self.assertEqual(second.filename, "scan_page_2.jpg")
                    self.assertTrue(second.image_data.startswith(b"\xff\xd8"))
                    self.assertEqual(render.call_count, 2)

                    with self.assertRaises(StopIteration):
                        next(stream)
        finally:
            if stream is not None:
                stream.close()
            service.close()

    def test_cancelling_after_metadata_removes_temporary_pdf(self):
        config = ReadConfig()
        config.parser_engine_overrides["pdf_force_scanned"] = "true"
        request = ReadRequest(
            file_content=_scanned_pdf(2),
            file_name="scan.pdf",
            file_type="pdf",
            config=config,
        )
        service = DocReaderServicer(preprocess_runner=_NoopPreprocessRunner())
        real_write = pdf_parser._write_pdf_temp_file
        temp_paths = []
        stream = None

        def record_temp_path(content):
            path = real_write(content)
            temp_paths.append(path)
            return path

        try:
            with patch.object(
                pdf_parser,
                "CONFIG",
                replace(pdf_parser.CONFIG, pdf_render_parallelism=1),
            ):
                with patch.object(
                    pdf_parser,
                    "_write_pdf_temp_file",
                    side_effect=record_temp_path,
                ):
                    stream = service.ReadStream(request, None)
                    self.assertEqual(next(stream).meta.image_count, 2)
                    self.assertEqual(len(temp_paths), 1)
                    self.assertTrue(os.path.exists(temp_paths[0]))
                    stream.close()
                    self.assertFalse(os.path.exists(temp_paths[0]))
        finally:
            if stream is not None:
                stream.close()
            service.close()


if __name__ == "__main__":
    unittest.main()
