from const import sanitize_rtsp_path


def test_simple_name():
    assert sanitize_rtsp_path("Erik", "abc123") == "Erik"


def test_name_with_spaces():
    assert sanitize_rtsp_path("Baby Room", "abc123") == "Baby_Room"


def test_name_with_slash():
    assert sanitize_rtsp_path("Baby/Room", "abc123") == "Baby_Room"


def test_name_with_backslash():
    assert sanitize_rtsp_path("Baby\\Room", "abc123") == "Baby_Room"


def test_empty_name_falls_back_to_id():
    assert sanitize_rtsp_path("", "abc123") == "abc123"


def test_underscore_only_falls_back_to_id():
    # Name made entirely of separators sanitizes to "_" → use id instead
    assert sanitize_rtsp_path("/", "abc123") == "abc123"


def test_multiple_spaces():
    assert sanitize_rtsp_path("Erik  Two", "abc123") == "Erik__Two"
