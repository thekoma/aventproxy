"""Tests for Tuya data-center routing (issues #44, #58)."""

import region
from region import (
    COUNTRY_ROUTING,
    DATA_CENTER_HOSTS,
    DEFAULT_DATA_CENTER,
    PROBE_ORDER,
    api_host,
    api_url,
    api_url_for_host,
    data_center_from_host,
    hosts_from_domain,
    is_wrong_data_center,
    login_candidates,
    normalize_api_host,
)


class TestDataCenterHosts:
    def test_every_probed_data_center_has_a_host(self):
        for name in PROBE_ORDER:
            assert name in DATA_CENTER_HOSTS

    def test_api_url_is_the_mobile_sdk_endpoint(self):
        assert api_url("eu") == "https://a1.tuyaeu.com/api.json"
        assert api_url("us") == "https://a1.tuyaus.com/api.json"
        assert api_url("in") == "https://a1.tuyain.com/api.json"
        assert api_url("cn") == "https://a1.tuyacn.com/api.json"

    def test_unknown_data_center_falls_back_to_default(self):
        assert api_url("mars") == api_url(DEFAULT_DATA_CENTER)
        assert api_host("") == DATA_CENTER_HOSTS[DEFAULT_DATA_CENTER]

    def test_api_host_has_no_scheme_or_path(self):
        host = api_host("us")
        assert host == "a1.tuyaus.com"
        assert "://" not in host and "/" not in host

    def test_host_round_trips_to_its_data_center(self):
        for name, host in DATA_CENTER_HOSTS.items():
            assert data_center_from_host(host) == name

    def test_unknown_host_maps_to_default(self):
        assert data_center_from_host("a1.example.com") == DEFAULT_DATA_CENTER


class TestCountryRouting:
    def test_italy_stays_on_the_eu_data_center(self):
        # The pre-fix hard-coded behaviour, which must not regress.
        assert COUNTRY_ROUTING["IT"] == ("39", "eu")

    def test_americas_route_to_the_us_data_center(self):
        for country in ("US", "CA", "MX", "BR"):
            assert COUNTRY_ROUTING[country][1] == "us"

    def test_india_and_china_have_their_own_data_centers(self):
        assert COUNTRY_ROUTING["IN"][1] == "in"
        assert COUNTRY_ROUTING["CN"][1] == "cn"

    def test_reporter_countries_are_covered(self):
        # Issue #44 reporters: United States and the Netherlands.
        assert COUNTRY_ROUTING["US"] == ("1", "us")
        assert COUNTRY_ROUTING["NL"] == ("31", "eu")

    def test_calling_codes_are_digit_strings(self):
        for country, (calling_code, data_center) in COUNTRY_ROUTING.items():
            assert calling_code.isdigit(), country
            assert not calling_code.startswith("0"), country
            assert data_center in DATA_CENTER_HOSTS, country

    def test_country_keys_are_uppercase_alpha2(self):
        for country in COUNTRY_ROUTING:
            assert len(country) == 2 and country.isupper()

    def test_table_covers_the_world(self):
        # The country field is a required selector, so a missing country would
        # make setup impossible for those users.
        assert len(COUNTRY_ROUTING) > 200

    def test_single_digit_calling_codes_are_only_the_real_ones(self):
        # Guards against a generation artefact truncating a calling code to the
        # first digit (e.g. Vatican as "3" instead of "39").
        singles = {c for c, (code, _) in COUNTRY_ROUTING.items() if len(code) == 1}
        assert {COUNTRY_ROUTING[c][0] for c in singles} <= {"1", "7"}

    def test_known_calling_codes(self):
        expected = {
            "AU": "61", "BR": "55", "CA": "1", "CH": "41", "DE": "49", "ES": "34",
            "FR": "33", "GB": "44", "IN": "91", "JP": "81", "MX": "52", "PL": "48",
            "RU": "7", "SE": "46", "US": "1", "VA": "39", "ZA": "27",
        }
        for country, calling_code in expected.items():
            assert COUNTRY_ROUTING[country][0] == calling_code, country


class TestLoginCandidates:
    def test_known_country_is_tried_first(self):
        assert login_candidates("US")[0] == ("us", "1")
        assert login_candidates("IT")[0] == ("eu", "39")

    def test_country_is_case_insensitive(self):
        assert login_candidates("us") == login_candidates("US")

    def test_known_country_keeps_its_calling_code_on_every_probe(self):
        # Only the data center is in doubt, so the account's own calling code
        # is reused across the fallbacks.
        candidates = login_candidates("NL")
        assert all(calling_code == "31" for _, calling_code in candidates)

    def test_every_data_center_is_probed_exactly_once(self):
        for country in ("US", "IT", "IN", None, "ZZ"):
            centers = [data_center for data_center, _ in login_candidates(country)]
            assert sorted(centers) == sorted(PROBE_ORDER)

    def test_unknown_country_probes_in_default_order(self):
        assert login_candidates(None) == [
            ("eu", "39"), ("us", "1"), ("in", "91"), ("cn", "86"),
        ]
        assert login_candidates("ZZ") == login_candidates(None)
        assert login_candidates("") == login_candidates(None)

    def test_candidates_are_usable_as_api_urls(self):
        for data_center, _ in login_candidates("BR"):
            assert api_url(data_center).startswith("https://a1.tuya")


class TestWrongDataCenterDetection:
    def test_session_and_missing_user_codes_trigger_a_retry(self):
        assert is_wrong_data_center("USER_SESSION_INVALID")
        assert is_wrong_data_center("USER_NOT_EXISTS")
        assert is_wrong_data_center("REGION_NOT_MATCH")

    def test_credential_failures_never_trigger_a_retry(self):
        # Retrying these across data centers would burn failed-login attempts
        # on the account, so they must stop the probe.
        assert not is_wrong_data_center("USER_PASSWD_WRONG")
        assert not is_wrong_data_center("USER_PASSWD_WRONG_TOO_MANY_TIME")
        assert not is_wrong_data_center("MFA_NEED_SEND_CODE")
        assert not is_wrong_data_center("MFA_CODE_INVALID")
        assert not is_wrong_data_center("WRONG_CODE")

    def test_unrelated_codes_do_not_trigger_a_retry(self):
        assert not is_wrong_data_center("RATE_LIMIT")
        assert not is_wrong_data_center("UNKNOWN")
        assert not is_wrong_data_center("")

    def test_detection_is_case_insensitive(self):
        assert is_wrong_data_center("user_session_invalid")


class TestDomainBlock:
    LOGIN_DOMAIN = {
        "mobileApiUrl": "a1.tuyaeu.com",
        "mobileMqttsUrl": "m1.tuyaeu.com",
        "regionCode": "EU",
        "httpsPort": 443,
    }

    def test_hosts_are_extracted_from_the_login_domain(self):
        assert hosts_from_domain(self.LOGIN_DOMAIN) == {
            "api_host": "a1.tuyaeu.com",
            "mqtt_host": "m1.tuyaeu.com",
            "region_code": "EU",
        }

    def test_non_eu_account_domain(self):
        hosts = hosts_from_domain({
            "mobileApiUrl": "https://a1.tuyaus.com/api.json",
            "mobileMqttsUrl": "m1.tuyaus.com",
            "regionCode": "AZ",
        })
        assert hosts["api_host"] == "a1.tuyaus.com"
        assert hosts["mqtt_host"] == "m1.tuyaus.com"
        assert hosts["region_code"] == "AZ"

    def test_missing_or_malformed_domain_yields_nothing(self):
        assert hosts_from_domain(None) == {}
        assert hosts_from_domain("a1.tuyaeu.com") == {}
        assert hosts_from_domain({}) == {}
        assert hosts_from_domain({"mobileApiUrl": ""}) == {}

    def test_partial_domain_keeps_what_is_present(self):
        assert hosts_from_domain({"mobileMqttsUrl": "m1.tuyain.com"}) == {
            "mqtt_host": "m1.tuyain.com",
        }

    def test_normalize_strips_scheme_and_path(self):
        assert normalize_api_host("https://a1.tuyaus.com/api.json") == "a1.tuyaus.com"
        assert normalize_api_host("http://a1.tuyain.com/") == "a1.tuyain.com"
        assert normalize_api_host("  a1.tuyacn.com  ") == "a1.tuyacn.com"
        assert normalize_api_host(None) is None
        assert normalize_api_host("") is None

    def test_api_url_for_host_round_trips_with_the_data_center_tables(self):
        for name, host in DATA_CENTER_HOSTS.items():
            assert api_url_for_host(host) == api_url(name)

    def test_api_url_for_host_falls_back_when_empty(self):
        assert api_url_for_host(None) == api_url(DEFAULT_DATA_CENTER)
        assert api_url_for_host("") == api_url(DEFAULT_DATA_CENTER)


class TestNoHomeAssistantImport:
    def test_module_is_importable_without_home_assistant(self):
        # The config flow needs HA; this module must not, so the routing can
        # be tested standalone.
        assert not hasattr(region, "homeassistant")
