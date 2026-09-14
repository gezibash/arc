defmodule Arc.Data.Handler do
  @moduledoc """
  Behaviour for service handlers.

  A handler receives messages and returns replies. Handlers are selected
  by URI scheme when starting `arc serve <uri>`:

    exec:///path/to/runtime?manifest=/abs/path/to/capability.(json|toml)

  Handlers may also implement `handle_message/4` to receive request context
  (frame metadata, arc session id, app session id, request id).
  Handlers may implement `capability/1` to describe their machine-readable
  launch manifest shape for ARC-native discovery.
  """

  @type state :: term()
  @type request_context :: %{
          required(:from_pk) => binary(),
          required(:meta) => map(),
          required(:frame_type) => Arc.Data.Frame.frame_type(),
          required(:arc_session_id) => binary() | nil,
          required(:app_session_id) => binary() | String.t() | nil,
          required(:request_id) => binary() | nil,
          required(:framed?) => boolean()
        }

  @type outbound_event :: %{
          required(:to_pk) => binary(),
          optional(:payload) => binary(),
          optional(:frame_type) => Arc.Data.Frame.frame_type(),
          optional(:request_id) => binary(),
          optional(:meta) => map(),
          optional(:body) => binary()
        }

  @doc """
  Build an outbound event for an `{:emit, [event], state}` return. The
  agent sends it to `to_pk` as a type 10 frame with `meta.topic`.
  """
  @spec emit_event(binary(), String.t(), binary()) :: outbound_event()
  def emit_event(<<to_pk::binary-size(32)>>, topic, body)
      when is_binary(topic) and is_binary(body) do
    %{to_pk: to_pk, payload: Arc.Data.Frame.encode_event(topic, body)}
  end

  @callback init(uri :: String.t()) :: {:ok, state()} | {:error, term()}
  @callback handle_message(message :: binary(), from_pk :: binary(), state()) ::
              {:reply, binary(), state()}
              | {:noreply, state()}
              | {:emit, [outbound_event()], state()}
  @callback handle_message(
              message :: binary(),
              from_pk :: binary(),
              context :: request_context(),
              state()
            ) ::
              {:reply, binary(), state()}
              | {:noreply, state()}
              | {:emit, [outbound_event()], state()}
  @callback handle_frame(
              frame_type :: Arc.Data.Frame.frame_type(),
              message :: binary(),
              from_pk :: binary(),
              context :: request_context(),
              state()
            ) ::
              {:reply, binary(), state()}
              | {:noreply, state()}
              | {:emit, [outbound_event()], state()}
  @callback handle_info(message :: term(), state()) ::
              :unhandled | {:noreply, state()} | {:emit, [outbound_event()], state()}
  @callback capability(state()) :: map()
  @callback package(state()) :: map()

  @optional_callbacks handle_message: 4,
                      handle_frame: 5,
                      handle_info: 2,
                      capability: 1,
                      package: 1

  @doc """
  Resolve a handler module from a URI string.
  """
  @spec resolve(String.t()) :: {:ok, module(), String.t()} | {:error, term()}
  def resolve(uri) do
    case URI.parse(uri) do
      %URI{scheme: "exec"} ->
        {:ok, Arc.Data.Handler.Exec, uri}

      %URI{scheme: scheme} when is_binary(scheme) ->
        {:error, {:unknown_scheme, scheme}}

      _ ->
        {:error, :invalid_uri}
    end
  end

  @spec package(module(), state()) :: map()
  def package(mod, state) do
    package =
      cond do
        function_exported?(mod, :package, 1) ->
          mod.package(state)

        function_exported?(mod, :capability, 1) ->
          Arc.Data.CapabilityPackage.wrap_capability(mod.capability(state))

        true ->
          %{}
      end

    normalize_package(package)
  end

  @doc """
  Return a normalized machine-readable capability description for a handler.
  """
  @spec capability(module(), state()) :: map()
  def capability(mod, state) do
    package(mod, state)["capability"] || %{}
  end

  @doc """
  Validate that a served handler publishes an explicit capability manifest.
  """
  @spec validate_served_capability(module(), state()) :: {:ok, map()} | {:error, term()}
  def validate_served_capability(mod, state) do
    if function_exported?(mod, :package, 1) or function_exported?(mod, :capability, 1) do
      package = package(mod, state)

      if Arc.Data.CapabilityPackage.valid?(package) do
        {:ok, package}
      else
        {:error, {:invalid_capability_manifest, mod}}
      end
    else
      {:error, {:missing_capability_manifest, mod}}
    end
  end

  defp normalize_package(package) when is_map(package) do
    if Map.has_key?(package, "capability") do
      Arc.Data.CapabilityPackage.normalize_package(package)
    else
      Arc.Data.CapabilityPackage.wrap_capability(package)
    end
  end

  defp normalize_package(_package), do: %{}
end
