defmodule Arc.Identity.HKDFTest do
  use ExUnit.Case, async: true

  alias Arc.Identity.HKDF

  # RFC 5869, appendix A.1
  test "matches the RFC 5869 test vector" do
    ikm = :binary.copy(<<0x0B>>, 22)
    salt = <<0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C>>
    info = <<0xF0, 0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8, 0xF9>>

    expected =
      Base.decode16!(
        "3CB25F25FAACD57A90434F64D0362F2A2D2D0A90CF1A5A4C5DB02D56ECC4C5BF34007208D5B887185865",
        case: :upper
      )

    assert HKDF.derive(ikm, salt, info, 42) == expected
  end

  test "empty salt equals a salt of 32 zero bytes" do
    assert HKDF.derive("ikm", "", "info", 32) == HKDF.derive("ikm", <<0::256>>, "info", 32)
  end

  test "returns exactly len bytes across block boundaries" do
    assert byte_size(HKDF.derive("ikm", "salt", "info", 33)) == 33
    assert byte_size(HKDF.derive("ikm", "salt", "info", 64)) == 64
  end
end
