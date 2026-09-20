defmodule Arc.Control do
  @moduledoc """
  Control plane behaviour and public API.

  The control plane answers: who is this identity, where are they,
  and are they still valid. Providers implement the 5-operation contract.

  Resolution is local-first: the configured provider (default: Local)
  handles publish/resolve/revoke. Chain adapters (Hedera, Nostr) come later.
  """

  alias Arc.Identity

  @type entry :: %{
          public_key: Identity.public_key(),
          name: String.t(),
          short_name: String.t(),
          published_at: integer(),
          status: :active | :revoked
        }

  @callback publish(Identity.t()) :: :ok | {:error, term()}
  @callback resolve(String.t()) :: {:ok, [entry()]} | {:error, term()}
  @callback revoke(Identity.public_key()) :: :ok | {:error, term()}
  @callback subscribe(atom()) :: {:ok, pid()} | {:error, term()}

  def publish(%Identity{} = identity), do: provider().publish(identity)
  def resolve(query) when is_binary(query), do: provider().resolve(query)

  def revoke(public_key), do: provider().revoke(public_key)
  def subscribe(event_type \\ :all), do: provider().subscribe(event_type)

  defp provider do
    Application.get_env(:arc_control, :provider, Arc.Control.Local)
  end
end
