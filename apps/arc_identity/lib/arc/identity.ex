defmodule Arc.Identity do
  @moduledoc """
  The identity axiom: every participant on ARC is a keypair first.

  A seed (32 bytes of entropy) deterministically derives an Ed25519 keypair.
  The seed is the identity — same seed, same keypair, any device.

  Each identity has a deterministic petname derived from its public key,
  e.g. "bold-einstein-3a7f0bc1". Short form: "bold-einstein".
  """

  @seed_bytes 32

  @type seed :: <<_::256>>
  @type public_key :: <<_::256>>
  @type secret_key :: <<_::256>>

  @type t :: %__MODULE__{
          seed: seed(),
          public_key: public_key(),
          secret_key: secret_key()
        }

  @enforce_keys [:seed, :public_key, :secret_key]
  defstruct [:seed, :public_key, :secret_key]

  @doc """
  Generate a new random identity.
  """
  @spec generate() :: t()
  def generate do
    seed = :crypto.strong_rand_bytes(@seed_bytes)
    from_seed(seed)
  end

  @doc """
  Derive an identity from an existing seed.
  Deterministic: same seed always produces the same identity.
  """
  @spec from_seed(seed()) :: t()
  def from_seed(<<seed::binary-size(@seed_bytes)>>) do
    {public_key, secret_key} = :crypto.generate_key(:eddsa, :ed25519, seed)

    %__MODULE__{
      seed: seed,
      public_key: public_key,
      secret_key: secret_key
    }
  end

  @doc """
  Sign a message with this identity's secret key.
  """
  @spec sign(t(), binary()) :: binary()
  def sign(%__MODULE__{secret_key: sk}, message) when is_binary(message) do
    :crypto.sign(:eddsa, :none, message, [sk, :ed25519])
  end

  @doc """
  Verify a signature against a public key.
  """
  @spec verify(public_key(), binary(), binary()) :: boolean()
  def verify(<<public_key::binary-size(32)>>, message, signature)
      when is_binary(message) and is_binary(signature) do
    :crypto.verify(:eddsa, :none, message, signature, [public_key, :ed25519])
  end

  @doc """
  Derive an X25519 keypair from this identity for ECDH key exchange.
  Returns {x25519_public, x25519_secret}.

  This is the standard conversion that libsodium calls
  `crypto_sign_ed25519_sk_to_curve25519`: the secret is the clamped first half
  of SHA-512 of the seed. The public key equals
  `public_key_to_x25519(identity.public_key)`, so any sender can compute it
  from the Ed25519 public key alone.
  """
  @spec to_x25519(t()) :: {binary(), binary()}
  def to_x25519(%__MODULE__{seed: seed}) do
    <<scalar::binary-size(32), _::binary-size(32)>> = :crypto.hash(:sha512, seed)
    <<first, middle::binary-size(30), last>> = scalar

    x_secret =
      <<Bitwise.band(first, 248), middle::binary, last |> Bitwise.band(127) |> Bitwise.bor(64)>>

    {x_public, _} = :crypto.generate_key(:ecdh, :x25519, x_secret)
    {x_public, x_secret}
  end

  # Field prime of Curve25519, 2^255 - 19.
  @p 57_896_044_618_658_097_711_785_492_504_343_953_926_634_992_332_820_282_019_728_792_003_956_564_819_949

  # u-coordinates of the points of small order. A key that maps to one of these
  # gives a shared secret that an attacker can predict. libsodium rejects the
  # same set.
  @small_order_u [
    0,
    1,
    325_606_250_916_557_431_795_983_626_356_110_631_294_008_115_727_848_805_560_023_387_167_927_233_504,
    39_382_357_235_489_614_581_723_060_781_553_021_112_529_911_719_440_698_176_882_885_853_963_445_705_823,
    @p - 1
  ]

  @doc """
  Compute the X25519 public key of an identity from its Ed25519 public key.

  This is the birational map from edwards25519 to Curve25519,
  u = (1 + y) / (1 - y) mod p. libsodium calls it
  `crypto_sign_ed25519_pk_to_curve25519`. The result equals the public half
  of `to_x25519/1` for the same identity.
  """
  @spec public_key_to_x25519(public_key()) :: {:ok, <<_::256>>} | {:error, :invalid_public_key}
  def public_key_to_x25519(<<encoded::little-unsigned-size(256)>>) do
    # The top bit holds the sign of x. The curve map uses y only.
    y = Bitwise.band(encoded, Bitwise.bsl(1, 255) - 1)

    with true <- y < @p and y != 1,
         u = rem((1 + y) * inverse(@p + 1 - y), @p),
         false <- u in @small_order_u do
      {:ok, <<u::little-unsigned-size(256)>>}
    else
      _ -> {:error, :invalid_public_key}
    end
  end

  def public_key_to_x25519(_), do: {:error, :invalid_public_key}

  # Fermat inverse: a^(p - 2) mod p.
  defp inverse(a), do: :crypto.mod_pow(rem(a, @p), @p - 2, @p) |> :binary.decode_unsigned()

  @doc """
  Encode a public key as a hex string.
  """
  @spec encode_public_key(t() | public_key()) :: String.t()
  def encode_public_key(%__MODULE__{public_key: pk}), do: Base.encode16(pk, case: :lower)
  def encode_public_key(<<pk::binary-size(32)>>), do: Base.encode16(pk, case: :lower)

  @doc """
  Full petname: "bold-einstein-3a7f0bc1"
  """
  @spec name(t() | public_key()) :: String.t()
  def name(%__MODULE__{public_key: pk}), do: Arc.Identity.Petname.from_public_key(pk)
  def name(<<pk::binary-size(32)>>), do: Arc.Identity.Petname.from_public_key(pk)

  @doc """
  Short petname: "bold-einstein"
  """
  @spec short_name(t() | public_key()) :: String.t()
  def short_name(%__MODULE__{public_key: pk}), do: Arc.Identity.Petname.short(pk)
  def short_name(<<pk::binary-size(32)>>), do: Arc.Identity.Petname.short(pk)
end
