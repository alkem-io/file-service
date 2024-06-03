export interface ConfigType {
  rabbitmq: {
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
      enabled: boolean;
      level: string;
      json: boolean;
    };
  };
  settings: {
    application: {
      storage: {
        local_storage_path: string;
        mapped_storage_path: string;
      };
      address: string;
      port: number;
      auth_queue: string;
      response_timeout: number;
    };
  };
}
