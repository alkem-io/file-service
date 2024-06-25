export interface ConfigType {
  rabbitmq: {
    /** # Connection in the form of 'amqp://[user]:[password]@[host]:[port]?heartbeat=[heartbeat]' */
    connection: {
      host: string;
      port: number;
      user: string;
      password: string;
      heartbeat: number;
    };
  };
  monitoring: {
    logging: {
      /** A flag setting whether Winston Console transport will be enabled.
       If the flag is set to true logs of the appropriate level (see below) will be outputted to the console
       after the application has been bootstrapped.
       The NestJS bootstrap process is handled by the internal NestJS logging.
       */
      enabled: boolean;
      /** Logging level for outputs to console.
       Valid values are log|error|warn|debug|verbose. */
      level: string;
      /** The logging format will be in json - useful for parsing
       if disabled - will be in a human-readable form */
      json: boolean;
    };
  };
  settings: {
    /**  */
    application: {
      /**  */
      storage: {
        /** Absolute path to the local storage of the files */
        storage_path: string;
      };
      /**  */
      address: string;
      /** The port on which the service is running  */
      port: number;
      /** authentication & authorization queue */
      auth_queue: string;
      /** MILLISECONDS wait time for a response after a request on the message queue */
      response_timeout: number;
      /** TTL in seconds, how much time a document should be cached for
      The service tell browser clients to cache the resource via the 'Cache-Control' and 'Etag' headers */
      document_max_age: number;
    };
  };
}
