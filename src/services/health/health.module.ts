import { Module } from '@nestjs/common';
import { FileModule } from '../file-reader';
import { HealthController } from './health.controller';

@Module({
  imports: [FileModule],
  controllers: [HealthController],
})
export class HealthModule {}
